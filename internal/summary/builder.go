package summary

import (
	"fmt"
	"sort"
	"strings"

	"insurance-benefit-agent-go/internal/eligibility"
	"insurance-benefit-agent-go/internal/enrichment"
	"insurance-benefit-agent-go/internal/models"
)

func Build(appointment models.Appointment, el *eligibility.PatientEligibility, enriched *enrichment.Enrichment) *Document {
	if el == nil {
		return nil
	}

	doc := &Document{
		Title: "Insurance Verification Summary",
		Patient: Patient{
			FullName: normalizeSpace(el.Patient.FullName),
			DOB:      normalizeSpace(el.Patient.DateOfBirth),
			MemberID: normalizeSpace(el.Patient.MemberID),
			Type:     normalizeSpace(el.Patient.MemberType),
			Status:   summarizeStatus(el),
		},
		Plan: Plan{
			Carrier:     normalizeSpace(el.Plan.Carrier),
			PlanName:    normalizeSpace(el.Plan.PlanName),
			GroupName:   normalizeSpace(el.Plan.GroupName),
			GroupNumber: normalizeSpace(el.Patient.GroupNumber),
			Network:     summarizeNetwork(el),
		},
		Visit: Visit{
			AppointmentDate: normalizeSpace(appointment.AppointmentDate),
			ProcedureCodes:  scheduledProcedureCodes(appointment),
		},
		Financial: Financial{
			DeductibleLines: accumulatorLines(el, "deductible"),
			MaximumLines:    accumulatorLines(el, "maximum"),
		},
		Metadata: Metadata{
			VerifiedAt: normalizeSpace(el.Metadata.EligibilityCheckedAt),
			Source:     normalizeSpace(el.Metadata.Source),
		},
	}

	doc.Highlights = buildHighlights(doc, el, enriched)
	doc.Alerts = buildAlerts(enriched)
	doc.NextSteps = buildNextSteps(doc, el, enriched)
	return doc
}

func summarizeStatus(el *eligibility.PatientEligibility) string {
	status := normalizeSpace(el.Patient.MemberEligibility)
	if status == "" {
		if el.Patient.IsEligible {
			status = "Active"
		} else {
			status = "Inactive"
		}
	}
	if el.Patient.EligibilityEffectiveDate != "" || el.Patient.EligibilityEndDate != "" {
		return fmt.Sprintf("%s (%s - %s)",
			status,
			firstNonEmpty(normalizeSpace(el.Patient.EligibilityEffectiveDate), "unknown"),
			firstNonEmpty(normalizeSpace(el.Patient.EligibilityEndDate), "open"),
		)
	}
	return status
}

func summarizeNetwork(el *eligibility.PatientEligibility) string {
	return firstNonEmpty(
		normalizeSpace(el.NetworkInfo.DisplayName),
		normalizeSpace(el.NetworkInfo.Type),
	)
}

func accumulatorLines(el *eligibility.PatientEligibility, kind string) []string {
	var lines []string
	for _, acc := range el.Accumulators {
		if !strings.EqualFold(normalizeSpace(acc.Kind), kind) {
			continue
		}
		label := normalizeSpace(acc.AccumulatorID)
		if label == "" {
			label = strings.Title(kind) //nolint:staticcheck
		}
		lines = append(lines, fmt.Sprintf("%s: total $%.2f | used $%.2f | remaining $%.2f", label, acc.Amount, acc.Used, acc.Remaining))
	}
	sort.Strings(lines)
	return lines
}

func buildHighlights(doc *Document, el *eligibility.PatientEligibility, enriched *enrichment.Enrichment) []string {
	var out []string
	if doc.Plan.PlanName != "" {
		out = append(out, "Plan: "+doc.Plan.PlanName)
	}
	if doc.Plan.Network != "" {
		out = append(out, "Network: "+doc.Plan.Network)
	}
	if len(doc.Visit.ProcedureCodes) > 0 {
		out = append(out, "Scheduled procedures: "+strings.Join(doc.Visit.ProcedureCodes, ", "))
	}
	if enriched != nil && len(enriched.SummaryFacts) > 0 {
		out = append(out, enriched.SummaryFacts...)
	}
	return distinctNonEmpty(out)
}

func buildAlerts(enriched *enrichment.Enrichment) []string {
	if enriched == nil || len(enriched.Alerts) == 0 {
		return nil
	}
	out := make([]string, 0, len(enriched.Alerts))
	for _, alert := range enriched.Alerts {
		line := firstNonEmpty(normalizeSpace(alert.Title), normalizeSpace(alert.Category))
		detail := normalizeSpace(alert.Detail)
		if detail != "" {
			line = line + ": " + detail
		}
		out = append(out, line)
	}
	return distinctNonEmpty(out)
}

func buildNextSteps(doc *Document, el *eligibility.PatientEligibility, enriched *enrichment.Enrichment) []string {
	var out []string
	if !el.Patient.IsEligible {
		out = append(out, "Re-verify coverage before scheduling treatment.")
	}
	if len(doc.Visit.ProcedureCodes) > 0 {
		out = append(out, "Review scheduled procedures against coverage and pre-auth rules.")
	}
	if len(el.Accumulators) > 0 {
		out = append(out, "Review deductible and maximum amounts before presenting treatment estimates.")
	}
	if enriched != nil && len(enriched.Alerts) > 0 {
		out = append(out, "Review office alerts before confirming treatment plan.")
	}
	return distinctNonEmpty(out)
}

func scheduledProcedureCodes(appointment models.Appointment) []string {
	if normalizeSpace(appointment.TreatmentPlanProcCodes) == "" {
		return nil
	}
	replacer := strings.NewReplacer(";", ",", "|", ",")
	parts := strings.Split(replacer.Replace(appointment.TreatmentPlanProcCodes), ",")
	var out []string
	for _, part := range parts {
		code := strings.ToUpper(normalizeSpace(part))
		if code != "" {
			out = append(out, code)
		}
	}
	return distinctNonEmpty(out)
}

func distinctNonEmpty(values []string) []string {
	seen := make(map[string]struct{})
	var out []string
	for _, value := range values {
		v := normalizeSpace(value)
		if v == "" {
			continue
		}
		if _, ok := seen[v]; ok {
			continue
		}
		seen[v] = struct{}{}
		out = append(out, v)
	}
	return out
}

func normalizeSpace(value string) string {
	return strings.Join(strings.Fields(value), " ")
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if normalizeSpace(value) != "" {
			return normalizeSpace(value)
		}
	}
	return ""
}
