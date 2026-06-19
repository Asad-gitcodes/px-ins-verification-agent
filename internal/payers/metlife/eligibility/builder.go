// Package eligibility maps MetLife API probe results into the payer-agnostic
// PatientEligibility model consumed by advanced.Build and the PDF renderer.
package eligibility

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"insurance-benefit-agent-go/internal/eligibility"
	"insurance-benefit-agent-go/internal/models"
	metlifeapi "insurance-benefit-agent-go/internal/payers/metlife/api"
)

// BuildFromProbe converts MetLife API probe results into the canonical PatientEligibility.
func BuildFromProbe(
	appointment models.Appointment,
	person *metlifeapi.CoveredPerson,
	planOverview *metlifeapi.PlanOverviewResponse,
	categories *metlifeapi.ProcedureCategoriesResponse,
	procedureCodeResponses map[string]*metlifeapi.ProcedureCodesResponse,
) *eligibility.PatientEligibility {
	if person == nil {
		return nil
	}

	el := &eligibility.PatientEligibility{
		Coverage: eligibility.Coverage{Categories: []eligibility.CoverageCategory{}},
		NetworkTiers: []eligibility.NetworkTier{
			{TierID: "in_network", DisplayName: "In-Network", IsContracted: true},
			{TierID: "out_network", DisplayName: "Out-of-Network", IsContracted: false},
		},
		NetworkMatrix:    []eligibility.NetworkMatrixRow{},
		Accumulators:     []eligibility.Accumulator{},
		OfficeSummary:    []eligibility.OfficeSummaryNote{},
		TreatmentHistory: make(map[string][]eligibility.TreatmentHistoryEntry),
	}

	applyPatient(el, person, appointment)
	applyPlan(el, person, planOverview, appointment)
	applyNetwork(el, person)

	if categories != nil && len(categories.Insureds) > 0 {
		insured := categories.Insureds[0]
		applyNetworkMatrix(el, insured)
		applyCoverageCategories(el, insured, procedureCodeResponses)
		applyTreatmentHistory(el, insured)
	}

	if planOverview != nil {
		applyAccumulators(el, planOverview)
	}

	el.Metadata = eligibility.Metadata{
		EligibilityCheckedAt: time.Now().UTC().Format(time.RFC3339),
		Source:               "MetLifeAPIProbe",
	}
	return el
}

// ── section appliers ──────────────────────────────────────────────────────────

func applyPatient(el *eligibility.PatientEligibility, p *metlifeapi.CoveredPerson, appt models.Appointment) {
	memberType := "Subscriber"
	if p.RelationShipCode != "0" {
		memberType = "Dependent"
	}
	el.Patient = eligibility.PatientInfo{
		FullName:                 strings.TrimSpace(p.FirstName + " " + p.LastName),
		MemberType:               memberType,
		DateOfBirth:              normalizeDate(p.DateOfBirth),
		MemberID:                 firstNonEmpty(p.EmployeeID, appt.SubscriberID),
		GroupNumber:              firstNonEmpty(p.GroupNumber, appt.GroupNum),
		MemberEligibility:        p.CoverageStatus,
		EligibilityEffectiveDate: normalizeDate(p.CoverageStartDate),
		EligibilityEndDate:       normalizeDate(p.CoverageEndDate),
		IsEligible:               strings.EqualFold(strings.TrimSpace(p.CoverageStatus), "active"),
	}
}

func applyPlan(el *eligibility.PatientEligibility, p *metlifeapi.CoveredPerson, plan *metlifeapi.PlanOverviewResponse, appt models.Appointment) {
	planName := strings.TrimSpace(p.Plan)
	if planName == "" && plan != nil {
		planName = strings.TrimSpace(plan.Plan.PlanCode)
	}
	el.Plan = eligibility.PlanInfo{
		Carrier:    "MetLife",
		PlanName:   planName,
		GroupName:  firstNonEmpty(strings.TrimSpace(appt.GroupName), strings.TrimSpace(p.Employer)),
		PlanDesign: strings.TrimSpace(p.Network),
		Provisions: make(map[string]string),
	}
	if plan != nil {
		for _, prov := range plan.PlanProvisions {
			if prov.Label != "" && len(prov.Text) > 0 {
				el.Plan.Provisions[prov.Label] = strings.Join(prov.Text, "; ")
			}
		}
		if s := strings.TrimSpace(plan.IncentiveData.PreAuthRequired); s != "" && !strings.EqualFold(s, "No") {
			el.Plan.Highlights = append(el.Plan.Highlights, "Pre-authorization required: "+s)
		}
		if s := strings.TrimSpace(plan.IncentiveData.PlanStartDate); s != "" {
			el.Plan.Provisions["Plan Start Date"] = s
		}
	}
}

func applyNetwork(el *eligibility.PatientEligibility, p *metlifeapi.CoveredPerson) {
	el.NetworkInfo = eligibility.NetworkInfo{
		Type:        p.NetworkID,
		DisplayName: firstNonEmpty(strings.TrimSpace(p.Network), "In-Network"),
		Confidence:  1,
		Reason:      "Resolved from MetLife eligibility overview",
	}
}

func applyNetworkMatrix(el *eligibility.PatientEligibility, insured metlifeapi.ProcedureCategoriesInsured) {
	for _, group := range insured.ProcedureCategoryGroups {
		if group.CategoryGroupName == "" || len(group.BenefitLevelRange) == 0 {
			continue
		}
		row := eligibility.NetworkMatrixRow{
			Name:   titleCase(group.CategoryGroupName),
			Values: make(map[string]string),
		}
		for _, blr := range group.BenefitLevelRange {
			row.Values[networkTypeToTierID(blr.NetworkTypeCode)] = blr.Range
		}
		el.NetworkMatrix = append(el.NetworkMatrix, row)
	}
}

func applyCoverageCategories(
	el *eligibility.PatientEligibility,
	insured metlifeapi.ProcedureCategoriesInsured,
	codeResponses map[string]*metlifeapi.ProcedureCodesResponse,
) {
	if codeResponses == nil {
		return
	}
	for _, group := range insured.ProcedureCategoryGroups {
		cat := eligibility.CoverageCategory{Name: titleCase(group.CategoryGroupName)}
		for _, procCat := range group.ProcedureCategories {
			resp, ok := codeResponses[procCat.TypeCode]
			if !ok || resp == nil || len(resp.Procedures) == 0 {
				continue
			}
			inNetDetail := findBenefitDetail(procCat.BenefitDetails, "INNETWORK")
			deductibleExempted := inNetDetail != nil &&
				strings.EqualFold(strings.TrimSpace(inNetDetail.DeductibleDesc), "NO")
			limitText := buildLimitText(procCat, inNetDetail)

			for _, proc := range resp.Procedures {
				svc := eligibility.CoverageService{
					Code:               strings.TrimSpace(proc.TypeCode),
					Description:        strings.TrimSpace(proc.Description),
					CoveragePercent:    parseBenefitLevel(proc.BenefitLevel),
					Limitations:        limitText,
					DeductibleExempted: deductibleExempted,
				}
				if proc.CoPayInd {
					svc.CopayAmount = strings.TrimSpace(proc.PatientObligation)
				}
				if al := strings.TrimSpace(procCat.AgeLimit); al != "" && al != "99" {
					svc.AgeLimits = "0 - " + al
				}
				cat.Services = append(cat.Services, svc)
			}
		}
		if len(cat.Services) > 0 {
			el.Coverage.Categories = append(el.Coverage.Categories, cat)
		}
	}
}

func applyTreatmentHistory(el *eligibility.PatientEligibility, insured metlifeapi.ProcedureCategoriesInsured) {
	for _, group := range insured.ProcedureCategoryGroups {
		for _, procCat := range group.ProcedureCategories {
			lastDate, ok := procCat.LastServiceDate.(string)
			if !ok || strings.TrimSpace(lastDate) == "" {
				continue
			}
			normalized := normalizeDate(lastDate)
			if normalized == "" {
				continue
			}
			code := strings.ToUpper(strings.TrimSpace(procCat.TypeCode))
			el.TreatmentHistory[code] = append(el.TreatmentHistory[code],
				eligibility.TreatmentHistoryEntry{ServiceDate: normalized},
			)
		}
	}
}

func applyAccumulators(el *eligibility.PatientEligibility, plan *metlifeapi.PlanOverviewResponse) {
	ann := plan.Maximums.Annual
	if total := parseMoney(ann.TotalAmount); total > 0 {
		el.Accumulators = append(el.Accumulators, eligibility.Accumulator{
			AccumulatorID: "annual-maximum",
			Name:          "Annual Maximum",
			Kind:          "maximum",
			Type:          "calendar",
			Scope:         "individual",
			Amount:        total,
			Used:          parseMoney(ann.UsedAmount),
			Remaining:     parseMoney(ann.RemainingAmount),
		})
	}

	for i, constraint := range plan.Maximums.Lifetime.Constraints {
		total := parseMoney(constraint.TotalAmount)
		if total == 0 {
			continue
		}
		name := "Lifetime Maximum"
		if constraint.ConstraintType != "" {
			name = "Lifetime " + titleCase(constraint.ConstraintType) + " Maximum"
		}
		el.Accumulators = append(el.Accumulators, eligibility.Accumulator{
			AccumulatorID: fmt.Sprintf("lifetime-maximum-%d", i),
			Name:          name,
			Kind:          "maximum",
			Type:          "lifetime",
			Scope:         "individual",
			Amount:        total,
			Used:          parseMoney(constraint.UsedAmount),
			Remaining:     parseMoney(constraint.RemainingAmount),
		})
	}

	indiv := plan.Deductibles.Individual
	if total := parseMoney(indiv.TotalAmount); total > 0 {
		el.Accumulators = append(el.Accumulators, eligibility.Accumulator{
			AccumulatorID: "deductible-individual",
			Name:          "Individual Deductible",
			Kind:          "deductible",
			Type:          "calendar",
			Scope:         "individual",
			Amount:        total,
			Used:          parseMoney(indiv.UsedAmount),
			Remaining:     parseMoney(indiv.RemainingAmount),
		})
	}

	fam := plan.Deductibles.Family
	if total := parseMoney(fam.TotalAmount); total > 0 {
		el.Accumulators = append(el.Accumulators, eligibility.Accumulator{
			AccumulatorID: "deductible-family",
			Name:          "Family Deductible",
			Kind:          "deductible",
			Type:          "calendar",
			Scope:         "family",
			Amount:        total,
			Used:          parseMoney(fam.UsedAmount),
			Remaining:     parseMoney(fam.RemainingAmount),
		})
	}
}

// ── helpers ───────────────────────────────────────────────────────────────────

func findBenefitDetail(details []metlifeapi.ProcedureBenefitDetail, networkTypeCode string) *metlifeapi.ProcedureBenefitDetail {
	for i := range details {
		if strings.EqualFold(details[i].NetworkTypeCode, networkTypeCode) {
			return &details[i]
		}
	}
	return nil
}

func buildLimitText(procCat metlifeapi.ProcedureCategory, inNetDetail *metlifeapi.ProcedureBenefitDetail) string {
	var parts []string
	if t := strings.TrimSpace(procCat.LimitsDescription); t != "" {
		parts = append(parts, t)
	}
	if inNetDetail != nil {
		if t := strings.TrimSpace(inNetDetail.LimitsDescription); t != "" {
			lower := strings.ToLower(t)
			dup := false
			for _, p := range parts {
				if strings.ToLower(p) == lower {
					dup = true
					break
				}
			}
			if !dup {
				parts = append(parts, t)
			}
		}
	}
	return strings.Join(parts, "; ")
}

func parseBenefitLevel(s string) int {
	s = strings.TrimSpace(s)
	lower := strings.ToLower(s)
	if s == "" || strings.Contains(lower, "n/avail") || strings.Contains(lower, "information not") {
		return -1
	}
	s = strings.TrimSuffix(strings.TrimSpace(s), "%")
	f, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil {
		return -1
	}
	return int(f)
}

func parseMoney(s string) float64 {
	s = strings.TrimSpace(s)
	s = strings.ReplaceAll(s, "$", "")
	s = strings.ReplaceAll(s, ",", "")
	f, _ := strconv.ParseFloat(s, 64)
	return f
}

func networkTypeToTierID(code string) string {
	switch strings.ToUpper(strings.TrimSpace(code)) {
	case "INNETWORK":
		return "in_network"
	case "OUTNETWORK":
		return "out_network"
	default:
		return strings.ToLower(strings.ReplaceAll(code, " ", "_"))
	}
}

func titleCase(s string) string {
	parts := strings.Fields(strings.ToLower(s))
	for i, p := range parts {
		if len(p) > 0 {
			parts[i] = strings.ToUpper(p[:1]) + p[1:]
		}
	}
	return strings.Join(parts, " ")
}

func normalizeDate(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	for _, layout := range []string{"2006-01-02", "01/02/2006", "1/2/2006", "01/02/06"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.Format("2006-01-02")
		}
	}
	return s
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}
