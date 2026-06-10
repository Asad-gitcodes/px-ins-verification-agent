package enrichment

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"insurance-benefit-agent-go/internal/eligibility"
)

func Build(el *eligibility.PatientEligibility) *Enrichment {
	if el == nil {
		return nil
	}

	enriched := &Enrichment{
		Coverage:         buildCoverageStatus(el),
		Plan:             buildPlanSnapshot(el),
		Network:          buildNetworkSnapshot(el),
		Financial:        buildFinancialSnapshot(el),
		Alerts:           buildAlerts(el),
		ProcedureHistory: buildProcedureHistory(el),
		ProcedureSignals: buildProcedureSignals(el),
		SummaryFacts:     buildSummaryFacts(el),
		Metadata: Metadata{
			GeneratedAt: time.Now().UTC().Format(time.RFC3339),
			Source:      "PatientEligibility",
		},
	}
	return enriched
}

func buildCoverageStatus(el *eligibility.PatientEligibility) CoverageStatus {
	label := normalizeSpace(el.Patient.MemberEligibility)
	if label == "" {
		if el.Patient.IsEligible {
			label = "Active"
		} else {
			label = "Inactive"
		}
	}
	return CoverageStatus{
		IsEligible:    el.Patient.IsEligible,
		StatusLabel:   label,
		MemberType:    normalizeSpace(el.Patient.MemberType),
		EffectiveDate: normalizeSpace(el.Patient.EligibilityEffectiveDate),
		EndDate:       normalizeSpace(el.Patient.EligibilityEndDate),
	}
}

func buildPlanSnapshot(el *eligibility.PatientEligibility) PlanSnapshot {
	highlights := append([]string(nil), el.Plan.Highlights...)
	sort.Strings(highlights)

	provisions := map[string]string{}
	for key, value := range el.Plan.Provisions {
		if normalizeSpace(value) == "" {
			continue
		}
		provisions[key] = normalizeSpace(value)
	}
	if len(provisions) == 0 {
		provisions = nil
	}

	return PlanSnapshot{
		Carrier:     normalizeSpace(el.Plan.Carrier),
		PlanName:    normalizeSpace(el.Plan.PlanName),
		GroupName:   normalizeSpace(el.Plan.GroupName),
		GroupNumber: normalizeSpace(el.Patient.GroupNumber),
		MemberID:    normalizeSpace(el.Patient.MemberID),
		PlanDesign:  normalizeSpace(el.Plan.PlanDesign),
		Highlights:  highlights,
		Provisions:  provisions,
	}
}

func buildNetworkSnapshot(el *eligibility.PatientEligibility) NetworkSnapshot {
	tierNames := make([]string, 0, len(el.NetworkTiers))
	for _, tier := range el.NetworkTiers {
		name := normalizeSpace(tier.DisplayName)
		if name == "" {
			name = normalizeSpace(tier.TierID)
		}
		if name != "" {
			tierNames = append(tierNames, name)
		}
	}

	matrixLabels := make([]string, 0, len(el.NetworkMatrix))
	for _, row := range el.NetworkMatrix {
		if name := normalizeSpace(row.Name); name != "" {
			matrixLabels = append(matrixLabels, name)
		}
	}

	sort.Strings(tierNames)
	sort.Strings(matrixLabels)

	return NetworkSnapshot{
		Type:         normalizeSpace(el.NetworkInfo.Type),
		DisplayName:  normalizeSpace(el.NetworkInfo.DisplayName),
		Confidence:   el.NetworkInfo.Confidence,
		Reason:       normalizeSpace(el.NetworkInfo.Reason),
		TierNames:    tierNames,
		MatrixLabels: matrixLabels,
	}
}

func buildFinancialSnapshot(el *eligibility.PatientEligibility) FinancialSnapshot {
	var deductibles []AccumulatorSummary
	var maximums []AccumulatorSummary

	for _, acc := range el.Accumulators {
		summary := AccumulatorSummary{
			Name:      normalizeSpace(acc.AccumulatorID),
			Type:      normalizeSpace(acc.Type),
			Amount:    acc.Amount,
			Used:      acc.Used,
			Remaining: acc.Remaining,
			AppliesTo: accumulatorAppliesTo(acc),
		}
		switch strings.ToLower(normalizeSpace(acc.Kind)) {
		case "deductible":
			deductibles = append(deductibles, summary)
		case "maximum":
			maximums = append(maximums, summary)
		}
	}

	sort.Slice(deductibles, func(i, j int) bool { return deductibles[i].Name < deductibles[j].Name })
	sort.Slice(maximums, func(i, j int) bool { return maximums[i].Name < maximums[j].Name })

	return FinancialSnapshot{
		Deductibles: deductibles,
		Maximums:    maximums,
	}
}

func accumulatorAppliesTo(acc eligibility.Accumulator) []string {
	if len(acc.AccumulatorTreatmentTypes) == 0 {
		return nil
	}
	values := make([]string, 0, len(acc.AccumulatorTreatmentTypes))
	for _, item := range acc.AccumulatorTreatmentTypes {
		if name := normalizeSpace(item.Name); name != "" {
			values = append(values, name)
		}
	}
	sort.Strings(values)
	return values
}

func buildAlerts(el *eligibility.PatientEligibility) []Alert {
	alerts := make([]Alert, 0, len(el.OfficeSummary)+2)
	if !el.Patient.IsEligible {
		alerts = append(alerts, Alert{
			Severity: "high",
			Category: "coverage",
			Title:    "Coverage inactive",
			Detail:   firstNonEmpty(normalizeSpace(el.Patient.MemberEligibility), "Patient is not currently eligible."),
		})
	}
	if endDate := normalizeSpace(el.Patient.EligibilityEndDate); endDate != "" {
		alerts = append(alerts, Alert{
			Severity: "medium",
			Category: "coverage",
			Title:    "Coverage end date present",
			Detail:   fmt.Sprintf("Coverage currently shows an end date of %s.", endDate),
		})
	}
	for _, note := range el.OfficeSummary {
		severity := normalizeSpace(note.Tone)
		if severity == "" {
			severity = "info"
		}
		detail := normalizeSpace(note.Text)
		if detail == "" {
			continue
		}
		alerts = append(alerts, Alert{
			Severity: severity,
			Category: "office_summary",
			Title:    "Office note",
			Detail:   detail,
		})
	}
	return alerts
}

func buildProcedureHistory(el *eligibility.PatientEligibility) []ProcedureHistory {
	if len(el.TreatmentHistory) == 0 {
		return nil
	}

	codes := make([]string, 0, len(el.TreatmentHistory))
	for code := range el.TreatmentHistory {
		codes = append(codes, code)
	}
	sort.Strings(codes)

	out := make([]ProcedureHistory, 0, len(codes))
	for _, code := range codes {
		entries := el.TreatmentHistory[code]
		history := ProcedureHistory{
			Code:            normalizeSpace(code),
			Count:           len(entries),
			LastServiceDate: latestServiceDate(entries),
			ToothCodes:      distinctToothCodes(entries),
		}
		out = append(out, history)
	}
	return out
}

func buildProcedureSignals(el *eligibility.PatientEligibility) []ProcedureSignal {
	if len(el.Coverage.Categories) == 0 {
		return nil
	}

	var out []ProcedureSignal
	for _, category := range el.Coverage.Categories {
		categoryName := normalizeSpace(category.Name)
		for _, service := range category.Services {
			lastDate, historyCount := historyStats(el.TreatmentHistory[service.Code])
			out = append(out, ProcedureSignal{
				Category:                 categoryName,
				Code:                     normalizeSpace(service.Code),
				Description:              normalizeSpace(service.Description),
				CoveragePercent:          service.CoveragePercent,
				HasHistory:               historyCount > 0,
				HistoryCount:             historyCount,
				LastServiceDate:          lastDate,
				Limitations:              normalizeSpace(service.Limitations),
				AgeLimits:                normalizeSpace(service.AgeLimits),
				PreAuthorizationRequired: service.PreAuthorizationRequired,
			})
		}
	}

	sort.Slice(out, func(i, j int) bool {
		if out[i].Category == out[j].Category {
			return out[i].Code < out[j].Code
		}
		return out[i].Category < out[j].Category
	})
	return out
}

func buildSummaryFacts(el *eligibility.PatientEligibility) []string {
	var facts []string

	if plan := normalizeSpace(el.Plan.PlanName); plan != "" {
		facts = append(facts, "Plan: "+plan)
	}
	if memberID := normalizeSpace(el.Patient.MemberID); memberID != "" {
		facts = append(facts, "Member ID: "+memberID)
	}
	if network := firstNonEmpty(normalizeSpace(el.NetworkInfo.DisplayName), normalizeSpace(el.NetworkInfo.Type)); network != "" {
		facts = append(facts, "Network: "+network)
	}
	if len(el.Accumulators) > 0 {
		facts = append(facts, fmt.Sprintf("Accumulators captured: %d", len(el.Accumulators)))
	}
	if len(el.TreatmentHistory) > 0 {
		facts = append(facts, fmt.Sprintf("Treatment history codes: %d", len(el.TreatmentHistory)))
	}
	if len(el.NetworkMatrix) > 0 {
		facts = append(facts, fmt.Sprintf("Benefit matrix categories: %d", len(el.NetworkMatrix)))
	}
	return facts
}

func historyStats(entries []eligibility.TreatmentHistoryEntry) (string, int) {
	if len(entries) == 0 {
		return "", 0
	}
	return latestServiceDate(entries), len(entries)
}

func latestServiceDate(entries []eligibility.TreatmentHistoryEntry) string {
	latest := ""
	for _, entry := range entries {
		date := normalizeSpace(entry.ServiceDate)
		if date > latest {
			latest = date
		}
	}
	return latest
}

func distinctToothCodes(entries []eligibility.TreatmentHistoryEntry) []string {
	if len(entries) == 0 {
		return nil
	}
	seen := make(map[string]struct{})
	var out []string
	for _, entry := range entries {
		code := normalizeSpace(entry.ToothCode)
		if code == "" {
			continue
		}
		if _, ok := seen[code]; ok {
			continue
		}
		seen[code] = struct{}{}
		out = append(out, code)
	}
	sort.Strings(out)
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
