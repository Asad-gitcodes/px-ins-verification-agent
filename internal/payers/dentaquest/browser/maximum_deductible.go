package browser

import (
	"fmt"
	"regexp"
	"strings"

	"insurance-benefit-agent-go/internal/payers/dentaquest/eligibility"
)

type maxDeductibleResult struct {
	UsedPayload           bool
	ParsedAccumulatorCount int
	AddedOfficeNotes      int
}

// applyMaximumDeductiblePayload reads the maximum-deductible XHR payload and
// appends structured accumulators (or office-summary notes for special cases).
func applyMaximumDeductiblePayload(s *Session, el *eligibility.PatientEligibility) maxDeductibleResult {
	stored := s.GetPayload("maximum-deductible")
	records := asSlice(stored.GetPayloadSlice())
	if len(records) == 0 {
		return maxDeductibleResult{}
	}

	result := maxDeductibleResult{UsedPayload: true}
	for _, raw := range records {
		record := asStringMap(raw)
		if record == nil {
			continue
		}
		cls := classifyMaxDeductibleRecord(record)
		if cls.specialCase || cls.kind == "" {
			el.OfficeSummary = append(el.OfficeSummary, buildMaxDeductibleOfficeNote(record, cls))
			result.AddedOfficeNotes++
			continue
		}
		el.Accumulators = append(el.Accumulators, buildMaxDeductibleAccumulator(record, cls))
		result.ParsedAccumulatorCount++
	}

	return result
}

// GetPayloadSlice is a helper used by applyMaximumDeductiblePayload to safely
// return the payload as []any regardless of whether it is nil.
func (cp *CapturedPayload) GetPayloadSlice() []any {
	if cp == nil {
		return nil
	}
	return asSlice(cp.Payload)
}

type maxDeductibleClassification struct {
	kind        string // "deductible" or "maximum"
	accType     string // "calendar" or "lifetime"
	specialCase bool
	reason      string
}

func classifyMaxDeductibleRecord(r map[string]any) maxDeductibleClassification {
	name := normalizeSpace(fmt.Sprint(r["benefitName"]))
	lower := strings.ToLower(name)

	if name == "" {
		return maxDeductibleClassification{specialCase: true, reason: "Unnamed maximum/deductible record"}
	}
	if strings.Contains(lower, "out of pocket") {
		t := "calendar"
		if strings.Contains(lower, "lifetime") {
			t = "lifetime"
		}
		return maxDeductibleClassification{
			kind:        "maximum",
			accType:     t,
			specialCase: true,
			reason:      "Out-of-pocket maximum is a member-liability cap, not a standard dental plan maximum.",
		}
	}
	if strings.Contains(lower, "deductible") {
		t := "calendar"
		if strings.Contains(lower, "lifetime") {
			t = "lifetime"
		}
		return maxDeductibleClassification{kind: "deductible", accType: t}
	}
	if strings.Contains(lower, "maximum") {
		t := "calendar"
		if strings.Contains(lower, "lifetime") {
			t = "lifetime"
		}
		return maxDeductibleClassification{kind: "maximum", accType: t}
	}
	return maxDeductibleClassification{
		accType:     "calendar",
		specialCase: true,
		reason:      "Benefit does not look like a standard deductible or annual/lifetime maximum.",
	}
}

func buildMaxDeductibleOfficeNote(r map[string]any, cls maxDeductibleClassification) eligibility.OfficeSummaryNote {
	amount := parseMoney(fmt.Sprint(r["benefitAmount"]))
	used := parseMoney(fmt.Sprint(r["benefitApplied"]))
	remaining := amount - used
	if remaining < 0 {
		remaining = 0
	}
	classes := normalizeTreatmentClassList(fmt.Sprint(r["treatmentClass"]))

	parts := []string{
		normalizeSpace(fmt.Sprint(r["benefitName"])),
		fmt.Sprintf("total %s", formatMoney(amount)),
		fmt.Sprintf("used %s", formatMoney(used)),
		fmt.Sprintf("remaining %s", formatMoney(remaining)),
	}
	if len(classes) > 0 {
		parts = append(parts, "applies to "+strings.Join(classes, ", "))
	}
	if v := normalizeSpace(fmt.Sprint(r["rolloverMaximum"])); v != "" && v != "<nil>" {
		parts = append(parts, "rollover "+v)
	}
	if cls.reason != "" {
		parts = append(parts, cls.reason)
	}
	return eligibility.OfficeSummaryNote{Tone: "warn", Text: strings.Join(parts, " | ")}
}

func buildMaxDeductibleAccumulator(r map[string]any, cls maxDeductibleClassification) eligibility.Accumulator {
	amount := parseMoney(fmt.Sprint(r["benefitAmount"]))
	used := parseMoney(fmt.Sprint(r["benefitApplied"]))
	remaining := amount - used
	if remaining < 0 {
		remaining = 0
	}
	classes := normalizeTreatmentClassList(fmt.Sprint(r["treatmentClass"]))

	treatmentTypes := make([]eligibility.AccumulatorTreatmentType, 0, len(classes))
	for _, c := range classes {
		treatmentTypes = append(treatmentTypes, eligibility.AccumulatorTreatmentType{Name: c})
	}

	name := normalizeSpace(fmt.Sprint(r["benefitName"]))
	id := toSlug(name)
	if id == "" {
		id = fmt.Sprintf("%s-%.0f", cls.kind, amount)
	}
	return eligibility.Accumulator{
		AccumulatorID:             id,
		Name:                      name,
		Kind:                      cls.kind,
		Type:                      cls.accType,
		Scope:                     parseScopeFromBenefitName(name),
		Amount:                    amount,
		Used:                      used,
		Remaining:                 remaining,
		AccumulatorTreatmentTypes: treatmentTypes,
	}
}

func parseScopeFromBenefitName(name string) string {
	lower := strings.ToLower(name)
	switch {
	case strings.Contains(lower, "family"):
		return "family"
	case strings.Contains(lower, "individual"):
		return "individual"
	default:
		return ""
	}
}

var reNonTreatmentCode = regexp.MustCompile(`\s+D\d{4}\s*-\s*D\d{4}$`)

func normalizeTreatmentClassList(value string) []string {
	v := normalizeSpace(value)
	if v == "" || v == "<nil>" {
		return nil
	}
	var out []string
	for _, item := range strings.Split(v, ",") {
		cleaned := strings.TrimSpace(reNonTreatmentCode.ReplaceAllString(item, ""))
		if cleaned != "" {
			out = append(out, cleaned)
		}
	}
	return out
}

func formatMoney(amount float64) string {
	return fmt.Sprintf("$%.2f", amount)
}
