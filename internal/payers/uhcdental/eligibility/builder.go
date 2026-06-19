// Package eligibility maps the raw UHC Dental API responses into the
// payer-agnostic PatientEligibility model.
package eligibility

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"insurance-benefit-agent-go/internal/eligibility"
	uhcapi "insurance-benefit-agent-go/internal/payers/uhcdental/api"
)

// Build constructs a PatientEligibility from the two UHC Dental API responses
// and the member info from the member search.
func Build(
	bs *uhcapi.BenefitSummaryResponse,
	uh *uhcapi.UtilizationHistoryResponse,
	mi *uhcapi.MemberInfo,
) *eligibility.PatientEligibility {
	if bs == nil || uh == nil || mi == nil {
		return nil
	}

	m := bs.Result.DentalBenefitsAndAccums.Member
	hist := uh.Result.DentalServiceHistory

	el := &eligibility.PatientEligibility{}

	// ── Patient ───────────────────────────────────────────────────────────────
	firstName := strings.TrimSpace(m.PatientName.FirstName)
	lastName := strings.TrimSpace(m.PatientName.LastName)
	el.Patient = eligibility.PatientInfo{
		FullName:                 strings.TrimSpace(firstName + " " + lastName),
		MemberType:               mapMemberRelationship(hist.MemberRelationship),
		DateOfBirth:              mi.BirthDate,
		MemberID:                 m.SubscriberID,
		GroupNumber:              m.GroupID,
		MemberEligibility:        mapEligibilityLabel(m.EligibilityIndicator),
		EligibilityEffectiveDate: m.MemberEligibilityEffectiveDate,
		EligibilityEndDate:       m.EligibilityTermDate,
		IsEligible:               strings.EqualFold(m.EligibilityIndicator, "Y"),
	}

	// ── Plan ─────────────────────────────────────────────────────────────────
	el.Plan = eligibility.PlanInfo{
		Carrier:    "UnitedHealthCare Dental",
		PlanName:   m.ProductID.CodeDesc,
		GroupName:  m.GroupName,
		PlanDesign: m.ProductPlanTypeDescription,
	}

	// ── Network ───────────────────────────────────────────────────────────────
	el.NetworkInfo = eligibility.NetworkInfo{
		Type:        m.ProductPlanType,
		DisplayName: m.ProductPlanType,
		Confidence:  100,
		Reason:      "from benefitsummary productPlanType",
	}

	// ── Network tiers from categoryLevelBenefits providerTypes ───────────────
	tierSeen := map[string]bool{}
	for _, cb := range bs.Result.DentalBenefitsAndAccums.CategoryLevelBenefits {
		pt := cb.ProviderType
		if tierSeen[pt] {
			continue
		}
		tierSeen[pt] = true
		el.NetworkTiers = append(el.NetworkTiers, eligibility.NetworkTier{
			TierID:       pt,
			DisplayName:  mapProviderTypeDisplay(pt),
			IsContracted: pt == "I",
		})
	}

	// ── Network matrix (coverage % per category per tier) ────────────────────
	// Build: category codeValue → { providerType → "XX%" }
	type matrixKey struct{ category, providerType string }
	categoryNames := map[string]string{} // codeValue → codeDesc
	matrixValues := map[string]map[string]string{}

	for _, cb := range bs.Result.DentalBenefitsAndAccums.CategoryLevelBenefits {
		cat := cb.ProcedureCategory.CodeValue
		categoryNames[cat] = cb.ProcedureCategory.CodeDesc
		if matrixValues[cat] == nil {
			matrixValues[cat] = map[string]string{}
		}
		pct := cb.CoveragePct
		if pct != "" {
			matrixValues[cat][cb.ProviderType] = pct + "%"
		}
	}

	// Preserve order using category order from first encounter.
	categoryOrder := []string{}
	seen := map[string]bool{}
	for _, cb := range bs.Result.DentalBenefitsAndAccums.CategoryLevelBenefits {
		cat := cb.ProcedureCategory.CodeValue
		if !seen[cat] {
			seen[cat] = true
			categoryOrder = append(categoryOrder, cat)
		}
	}
	for _, cat := range categoryOrder {
		el.NetworkMatrix = append(el.NetworkMatrix, eligibility.NetworkMatrixRow{
			Name:   categoryNames[cat],
			Values: matrixValues[cat],
		})
	}

	// ── Accumulators from planLevelBenefits ───────────────────────────────────
	// UHC provides one entry per providerType (I/O) for the annual maximum.
	// We use the In-Network entry as the primary accumulator.
	for _, plb := range bs.Result.DentalBenefitsAndAccums.PlanLevelBenefits {
		if plb.ProviderType != "I" {
			continue
		}
		info := plb.PlanLevelLimitInfo
		amount := parseFloat(info.LimitMemberMaxAmt)
		used := parseFloat(info.CurrYearLimitMemberAmtSatisfied)
		if amount <= 0 {
			continue
		}
		year := info.LimitCurrentYear
		if year == "" {
			year = strconv.Itoa(time.Now().Year())
		}
		el.Accumulators = append(el.Accumulators, eligibility.Accumulator{
			AccumulatorID: "annual-max-individual",
			Name:          "Calendar Individual Maximum (" + year + ")",
			Kind:          "maximum",
			Type:          "calendar",
			Scope:         "individual",
			Amount:        amount,
			Used:          used,
			Remaining:     amount - used,
		})

		// Prior year maximum for reference (no used amount — informational).
		prevAmount := parseFloat(info.PrevYearOOPMemberMaximumAmount)
		prevYear := info.LimitPreviousYear
		if prevAmount > 0 && prevYear != "" {
			prevUsed := parseFloat(info.PrevYearLimitMemberAmtSatisfied)
			el.Accumulators = append(el.Accumulators, eligibility.Accumulator{
				AccumulatorID: "annual-max-individual-prev",
				Name:          "Calendar Individual Maximum (" + prevYear + ")",
				Kind:          "maximum",
				Type:          "calendar",
				Scope:         "individual",
				Amount:        prevAmount,
				Used:          prevUsed,
				Remaining:     prevAmount - prevUsed,
			})
		}
	}

	// ── Coverage (from utilizationHistory procedures) ─────────────────────────
	// Build a category → coverage% map from benefitsummary for in-network.
	catCoverage := map[string]int{} // procedureCategory codeValue → int %
	for _, cb := range bs.Result.DentalBenefitsAndAccums.CategoryLevelBenefits {
		if cb.ProviderType != "I" {
			continue
		}
		if pct, err := strconv.Atoi(strings.TrimSpace(cb.CoveragePct)); err == nil {
			catCoverage[cb.ProcedureCategory.CodeValue] = pct
		}
	}

	// Group procedures by category to build Coverage.Categories.
	type categoryEntry struct {
		name     string
		services []eligibility.CoverageService
	}
	categoryMap := map[string]*categoryEntry{}
	categoryOrder2 := []string{}

	for _, proc := range hist.Procedures {
		code := strings.TrimSpace(proc.Procedure.CodeValue)
		if code == "" {
			continue
		}

		cat := proc.ProcedureCategory
		if _, exists := categoryMap[cat]; !exists {
			categoryMap[cat] = &categoryEntry{name: categoryNameForCode(cat, bs)}
			categoryOrder2 = append(categoryOrder2, cat)
		}

		pct, _ := catCoverage[cat]
		notCovered := strings.EqualFold(proc.InNetworkFrequency, "No Coverage") ||
			strings.EqualFold(proc.InNetworkFrequency, "Invalid Procedure")
		if notCovered {
			pct = 0
		}

		relatedCodes := splitRelatedCodes(proc.RelatedCode, code)

		svc := eligibility.CoverageService{
			Code:             code,
			Description:      proc.Procedure.CodeDesc,
			CoveragePercent:  pct,
			Limitations:      buildLimitationText(proc),
			AgeLimits:        normalizeAgeLimit(proc.AgeLimit),
			CrossCheckCodes:  relatedCodes,
			FrequencyNetworks: "I,O", // UHC provides same freq for both networks
		}

		categoryMap[cat].services = append(categoryMap[cat].services, svc)
	}

	for _, cat := range categoryOrder2 {
		entry := categoryMap[cat]
		el.Coverage.Categories = append(el.Coverage.Categories, eligibility.CoverageCategory{
			Name:     entry.name,
			Services: entry.services,
		})
	}

	// ── Treatment history (from services[] in utilizationHistory) ─────────────
	el.TreatmentHistory = map[string][]eligibility.TreatmentHistoryEntry{}
	for _, proc := range hist.Procedures {
		code := strings.TrimSpace(proc.Procedure.CodeValue)
		if code == "" || len(proc.Services) == 0 {
			continue
		}
		for _, svc := range proc.Services {
			el.TreatmentHistory[code] = append(el.TreatmentHistory[code], eligibility.TreatmentHistoryEntry{
				ServiceDate: svc.ServiceDate,
				ToothCode:   svc.ToothRange,
				Surfaces:    svc.ToothSurface,
			})
		}
	}

	// ── Metadata ──────────────────────────────────────────────────────────────
	el.Metadata = eligibility.Metadata{
		EligibilityCheckedAt: time.Now().UTC().Format(time.RFC3339),
		Source:               "UHCDentalAPIProbe",
	}

	return el
}

// ── helpers ───────────────────────────────────────────────────────────────────

func mapMemberRelationship(rel string) string {
	if strings.EqualFold(rel, "SUBSCRIBER") {
		return "Subscriber"
	}
	return "Dependent"
}

func mapEligibilityLabel(indicator string) string {
	if strings.EqualFold(indicator, "Y") {
		return "Active"
	}
	return "Inactive"
}

func mapProviderTypeDisplay(pt string) string {
	switch pt {
	case "I":
		return "In-Network"
	case "O":
		return "Out-of-Network"
	default:
		return pt
	}
}

// buildLimitationText converts the UHC frequency fields into a limitation string
// compatible with the advanced package's ParseFrequency parser.
//
// UHC format: "2 - P - 1Y"  →  "{count} per {scope} per {period}"
// Special:    "No Coverage", "Invalid Procedure", "999 -   - 0M" (unlimited)
func buildLimitationText(proc uhcapi.UHCProcedure) string {
	freq := strings.TrimSpace(proc.InNetworkFrequency)
	if freq == "" || strings.EqualFold(freq, "No Coverage") ||
		strings.EqualFold(freq, "Invalid Procedure") {
		return ""
	}
	// "999 -   - 0M" means no numeric limit — omit frequency text.
	if strings.HasPrefix(freq, "999") {
		return ""
	}
	// Parse "count - scope - period" into human-readable limitation text.
	parts := strings.SplitN(freq, "-", 3)
	if len(parts) != 3 {
		return freq
	}
	count := strings.TrimSpace(parts[0])
	scope := strings.TrimSpace(parts[1])
	period := strings.TrimSpace(parts[2])

	scopeLabel := "per patient"
	if strings.EqualFold(scope, "F") {
		scopeLabel = "per tooth"
	}

	periodLabel := mapPeriodLabel(period)
	return fmt.Sprintf("%s time(s) %s %s", count, scopeLabel, periodLabel)
}

func mapPeriodLabel(period string) string {
	period = strings.ToUpper(strings.TrimSpace(period))
	switch {
	case period == "1Y":
		return "per calendar year"
	case period == "2Y":
		return "every 2 years"
	case period == "3Y":
		return "every 3 years"
	case period == "5Y":
		return "every 5 years"
	case strings.HasSuffix(period, "Y"):
		n := strings.TrimSuffix(period, "Y")
		return "every " + n + " years"
	case strings.HasSuffix(period, "M"):
		n := strings.TrimSuffix(period, "M")
		return "every " + n + " months"
	case period == "1D":
		return "per day"
	case strings.HasSuffix(period, "Y") && strings.HasPrefix(period, "999"):
		return "per lifetime"
	default:
		return "per " + period
	}
}

func normalizeAgeLimit(ageLimit string) string {
	// UHC format: "0 - 999" or "11 - 999" or "3 - 999"
	// "0 - 999" means all ages — return empty (no restriction).
	ageLimit = strings.TrimSpace(ageLimit)
	if ageLimit == "" || ageLimit == "0 - 999" {
		return ""
	}
	return ageLimit
}

func splitRelatedCodes(relatedCode, selfCode string) []string {
	if relatedCode == "" {
		return nil
	}
	var out []string
	for _, c := range strings.Split(relatedCode, ",") {
		c = strings.TrimSpace(c)
		if c != "" && !strings.EqualFold(c, selfCode) {
			out = append(out, c)
		}
	}
	return out
}

func categoryNameForCode(catCode string, bs *uhcapi.BenefitSummaryResponse) string {
	for _, cb := range bs.Result.DentalBenefitsAndAccums.CategoryLevelBenefits {
		if cb.ProcedureCategory.CodeValue == catCode {
			return cb.ProcedureCategory.CodeDesc
		}
	}
	return catCode
}

func parseFloat(s string) float64 {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0
	}
	f, _ := strconv.ParseFloat(s, 64)
	return f
}

