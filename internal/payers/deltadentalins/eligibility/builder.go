// Package eligibility maps the raw Delta Dental API bundle into the
// payer-agnostic PatientEligibility model.
package eligibility

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"insurance-benefit-agent-go/internal/eligibility"
	ddapi "insurance-benefit-agent-go/internal/payers/deltadentalins/api"
)

// BuildEligibilityFromProbeBundle converts a Delta Dental PatientAPIBundle
// into the canonical PatientEligibility.  Returns nil when bundle is nil.
func BuildEligibilityFromProbeBundle(bundle *ddapi.PatientAPIBundle) *eligibility.PatientEligibility {
	if bundle == nil {
		return nil
	}

	el := baseEligibility(bundle)
	applySearchResult(bundle, el)
	applyActiveCoverage(bundle, el)
	applyMaxMemberInfo(bundle, el)
	applyBenefitsPackage(bundle, el)
	applyMaximumsDeductibles(bundle, el)
	applyTreatmentHistory(bundle, el)
	applyAdditionalBenefits(bundle, el)

	el.Metadata = eligibility.Metadata{
		EligibilityCheckedAt: time.Now().UTC().Format(time.RFC3339),
		Source:               "DeltaDentalAPIProbe",
	}
	return el
}

// ── base ─────────────────────────────────────────────────────────────────────

func baseEligibility(bundle *ddapi.PatientAPIBundle) *eligibility.PatientEligibility {
	appt := bundle.Appointment
	fullName := strings.TrimSpace(appt.FName + " " + appt.LName)

	memberType := "Subscriber"
	if strings.EqualFold(appt.Relationship, "dependent") {
		memberType = "Dependent"
	}

	return &eligibility.PatientEligibility{
		Patient: eligibility.PatientInfo{
			FullName:    fullName,
			MemberType:  memberType,
			DateOfBirth: appt.DOB,
			MemberID:    appt.SubscriberID,
			IsEligible:  true,
		},
		Plan: eligibility.PlanInfo{
			Carrier:    "Delta Dental",
			Provisions: make(map[string]string),
		},
		Coverage:      eligibility.Coverage{Categories: []eligibility.CoverageCategory{}},
		NetworkTiers:  []eligibility.NetworkTier{},
		NetworkMatrix: []eligibility.NetworkMatrixRow{},
		Accumulators:  []eligibility.Accumulator{},
		OfficeSummary: []eligibility.OfficeSummaryNote{},
	}
}

// ── search result ─────────────────────────────────────────────────────────────

func applySearchResult(bundle *ddapi.PatientAPIBundle, el *eligibility.PatientEligibility) {
	sr := bundle.SearchResult
	if name := strings.TrimSpace(sr.FirstName + " " + sr.LastName); name != "" {
		el.Patient.FullName = name
	}
	if sr.DateOfBirth != "" {
		el.Patient.DateOfBirth = sr.DateOfBirth
	}
	if sr.Card.MemberID != "" {
		el.Patient.MemberID = sr.Card.MemberID
	}
	if sr.Card.GroupNumber != "" {
		el.Patient.GroupNumber = sr.Card.GroupNumber
	}
	if sr.Card.SubscriberType != "" {
		el.Patient.MemberType = normalizeSubscriberType(sr.Card.SubscriberType)
	}
}

// ── active coverage ───────────────────────────────────────────────────────────

// applyActiveCoverage enriches eligibility from the winning plan coverage.
// Uses the same SelectWinningCoverage rules as the probe so Phase 1 and Phase 2
// always agree on which plan was selected.
func applyActiveCoverage(bundle *ddapi.PatientAPIBundle, el *eligibility.PatientEligibility) {
	sr := bundle.SearchResult
	if sr.Modal == nil || len(sr.Modal.Coverages) == 0 {
		el.Patient.IsEligible = sr.E1 != ""
		return
	}

	winner := ddapi.SelectWinningCoverage(sr.Modal.Coverages, bundle.Practice.DateOfService)
	if winner != nil {
		applyCoverage(*winner, el, true)
		return
	}

	// No coverage active on DOS — apply the first plan but mark as inactive.
	if len(sr.Modal.Coverages) > 0 {
		applyCoverage(sr.Modal.Coverages[0], el, false)
	}
}

// applyCoverage writes one PatientCoverage record into the eligibility result.
// When eligible is false the patient is explicitly marked inactive regardless
// of the MemberAccountStatus field.
func applyCoverage(cov ddapi.PatientCoverage, el *eligibility.PatientEligibility, eligible bool) {
	if cov.MemberID != "" {
		el.Patient.MemberID = cov.MemberID
	}
	if cov.GroupNumber != "" {
		el.Patient.GroupNumber = cov.GroupNumber
	}
	if cov.SubscriberType != "" {
		el.Patient.MemberType = normalizeSubscriberType(cov.SubscriberType)
	}
	if cov.Plan != "" {
		el.Plan.PlanName = cov.Plan
	}
	if cov.DivisionName != "" {
		el.Plan.GroupName = cov.DivisionName
	}

	el.Patient.MemberEligibility = cov.MemberAccountStatus
	if eligible {
		el.Patient.IsEligible = strings.EqualFold(cov.MemberAccountStatus, "Active") || cov.E1 != ""
	} else {
		el.Patient.IsEligible = false
	}

	if len(cov.EligibilitySpans) > 0 {
		el.Patient.EligibilityEffectiveDate = cov.EligibilitySpans[0].StartDate
		el.Patient.EligibilityEndDate = cov.EligibilitySpans[len(cov.EligibilitySpans)-1].EndDate
	}

	setProvision(el, "Coverage plan", cov.Plan)
	setProvision(el, "Division", cov.DivisionName)
	setProvision(el, "Member account status", cov.MemberAccountStatus)
	if cov.IsPrimaryPlan {
		setProvision(el, "Primary plan", "Yes")
	}
}

// ── max/member info ───────────────────────────────────────────────────────────

func applyMaxMemberInfo(bundle *ddapi.PatientAPIBundle, el *eligibility.PatientEligibility) {
	if bundle.MaximumsDeductibles == nil || bundle.MaximumsDeductibles.MemberInfo == nil {
		return
	}
	mi := bundle.MaximumsDeductibles.MemberInfo

	if mi.MemberName != "" && el.Patient.FullName == "" {
		el.Patient.FullName = mi.MemberName
	}
	if mi.BirthDate != "" && el.Patient.DateOfBirth == "" {
		el.Patient.DateOfBirth = mi.BirthDate
	}
	if mi.GroupNumber != "" && el.Patient.GroupNumber == "" {
		el.Patient.GroupNumber = mi.GroupNumber
	}
	if mi.BenefitPackageID != "" {
		setProvision(el, "Benefit package ID", mi.BenefitPackageID)
	}
	if mi.DefaultNetwork != "" {
		el.NetworkInfo.Type = mi.DefaultNetwork
		el.NetworkInfo.DisplayName = mi.DefaultNetwork
		el.NetworkInfo.Confidence = 1
		el.NetworkInfo.Reason = "Resolved from maximums-deductibles memberInfo.defaultNetwork"
		el.NetworkTiers = resolveNetworkTiers(mi.DefaultNetwork)
	}
	setProvision(el, "Contract ID", mi.ContractID)
	setProvision(el, "Default network", mi.DefaultNetwork)
	setProvision(el, "Division number", mi.DivisionNumber)
	setProvision(el, "Age", mi.Age)
}

// ── benefits package ──────────────────────────────────────────────────────────

func applyBenefitsPackage(bundle *ddapi.PatientAPIBundle, el *eligibility.PatientEligibility) {
	if len(bundle.BenefitsPackages) == 0 {
		return
	}
	pkg := bundle.BenefitsPackages[0]
	if pkg == nil {
		return
	}

	// defaultNetwork is resolved by applyMaxMemberInfo before this runs.
	defaultNetwork := el.NetworkInfo.Type

	for _, treat := range pkg.Treatment {
		cat := mapTreatmentToCategory(treat, defaultNetwork)
		if len(cat.Services) == 0 {
			continue
		}
		el.Coverage.Categories = append(el.Coverage.Categories, cat)
	}

	applyNetworkMatrix(pkg, el)
}

func mapTreatmentToCategory(treat ddapi.BenefitsTreatment, defaultNetwork string) eligibility.CoverageCategory {
	name := treat.TreatmentBusinessDescription
	if name == "" {
		name = treat.TreatmentDescription
	}
	if name == "" {
		name = treat.TreatmentCode
	}

	// Find the network index that matches the patient's default network (e.g. ##PPO).
	// Falls back to ##PPO, then index 0.
	networkIdx := networkIndexFor(treat.SummaryValues, defaultNetwork)

	cat := eligibility.CoverageCategory{Name: name}
	for _, cls := range treat.ProcedureClass {
		for _, proc := range cls.Procedure {
			svc := eligibility.CoverageService{
				Code:                     proc.Code,
				Description:              proc.Description,
				PreAuthorizationRequired: proc.PreApprovalRequired,
			}

			// Resolve the network entry for the patient's default network.
			var netEntry *ddapi.BenefitsNetwork
			if networkIdx >= 0 && networkIdx < len(proc.Network) {
				netEntry = &proc.Network[networkIdx]
			} else if len(proc.Network) > 0 {
				netEntry = &proc.Network[0]
			}

			if netEntry != nil {
				svc.CoveragePercent = pickCoveragePercent(netEntry.CoverageDetail)
				// Take exemption and copay from the first meaningful coverage detail.
				for _, cd := range netEntry.CoverageDetail {
					if cd.AmountType == "" && cd.BenefitCoverageLevel == "" {
						continue
					}
					svc.DeductibleExempted = cd.DeductibleExempted
					svc.MaximumExempted = cd.MaximumExempted
					svc.CopayAmount = cd.CopayAmount
					break
				}
				// Collect all frequency limitation texts from the default network.
				var limTexts []string
				for _, lim := range netEntry.Limitation {
					if t := strings.TrimSpace(lim.FrequencyLimitationText); t != "" {
						limTexts = append(limTexts, t)
					}
					if svc.FrequencyCounterID == "" {
						svc.FrequencyCounterID = lim.BenefitCounterIdentifier
					}
					if svc.FrequencyNetworks == "" {
						svc.FrequencyNetworks = lim.NetworksApplicable
					}
				}
				svc.Limitations = strings.Join(limTexts, "; ")
			}

			// CrossCheckCodes — split the comma-separated string.
			if proc.CrossCheckProcedureCodes != "" {
				for _, c := range strings.Split(proc.CrossCheckProcedureCodes, ",") {
					if c := strings.TrimSpace(c); c != "" {
						svc.CrossCheckCodes = append(svc.CrossCheckCodes, strings.ToUpper(c))
					}
				}
			}

			cat.Services = append(cat.Services, svc)
		}
	}
	return cat
}

// applyNetworkMatrix builds NetworkTiers and NetworkMatrix from the treatment
// summaryValues, which carry per-tier (##PPO / ##PMR / ##NP) coverage ranges.
func applyNetworkMatrix(pkg *ddapi.BenefitsPackageResponse, el *eligibility.PatientEligibility) {
	// Collect tier definitions from the first treatment that has summaryValues.
	var tiers []ddapi.BenefitsSummaryValue
	for _, treat := range pkg.Treatment {
		if len(treat.SummaryValues) > 0 {
			tiers = treat.SummaryValues
			break
		}
	}
	if len(tiers) == 0 {
		return
	}

	// Rebuild NetworkTiers from actual API data (overrides the generic 2-tier default).
	el.NetworkTiers = make([]eligibility.NetworkTier, 0, len(tiers))
	for _, sv := range tiers {
		el.NetworkTiers = append(el.NetworkTiers, eligibility.NetworkTier{
			TierID:       networkCodeToTierID(sv.NetworkCode),
			DisplayName:  networkCodeToDisplayName(sv.NetworkCode),
			IsContracted: sv.NetworkCode != "##NP",
		})
	}

	// One NetworkMatrixRow per treatment category.
	for _, treat := range pkg.Treatment {
		if len(treat.SummaryValues) == 0 {
			continue
		}
		name := treat.TreatmentBusinessDescription
		if name == "" {
			name = treat.TreatmentDescription
		}
		row := eligibility.NetworkMatrixRow{
			Name:   name,
			Values: make(map[string]string, len(treat.SummaryValues)),
		}
		for _, sv := range treat.SummaryValues {
			tierID := networkCodeToTierID(sv.NetworkCode)
			if sv.MinimumCoverage == sv.MaximumCoverage {
				row.Values[tierID] = fmt.Sprintf("%.0f%%", sv.MinimumCoverage)
			} else {
				row.Values[tierID] = fmt.Sprintf("%.0f%%–%.0f%%", sv.MinimumCoverage, sv.MaximumCoverage)
			}
		}
		el.NetworkMatrix = append(el.NetworkMatrix, row)
	}
}

// networkIndexFor returns the index in summaryValues whose NetworkCode matches
// defaultNetwork (case-insensitive). Falls back to ##PPO, then -1.
func networkIndexFor(summaryValues []ddapi.BenefitsSummaryValue, defaultNetwork string) int {
	// Exact match first.
	for i, sv := range summaryValues {
		if strings.EqualFold(sv.NetworkCode, defaultNetwork) {
			return i
		}
	}
	// Fall back to PPO.
	for i, sv := range summaryValues {
		if strings.EqualFold(sv.NetworkCode, "##PPO") {
			return i
		}
	}
	return -1
}

func networkCodeToTierID(code string) string {
	switch strings.ToUpper(code) {
	case "##PPO":
		return "ppo"
	case "##PMR":
		return "premier"
	case "##NP":
		return "out_network"
	default:
		return strings.ToLower(strings.TrimPrefix(code, "##"))
	}
}

func networkCodeToDisplayName(code string) string {
	switch strings.ToUpper(code) {
	case "##PPO":
		return "PPO"
	case "##PMR":
		return "Premier"
	case "##NP":
		return "Out-of-Network"
	default:
		return strings.TrimPrefix(code, "##")
	}
}

// pickCoveragePercent extracts an integer coverage percentage from the first
// meaningful CoverageDetail entry.
func pickCoveragePercent(details []ddapi.BenefitsCoverageDetail) int {
	for _, d := range details {
		if d.AmountType == "" && d.BenefitCoverageLevel == "" {
			continue
		}
		pct := parseCoverageLevel(d.BenefitCoverageLevel)
		if pct >= 0 {
			return pct
		}
	}
	return -1
}

// parseCoverageLevel converts strings like "100%", "80", "100.00", "Not Covered" to int.
// Returns -1 if the value cannot be parsed.
func parseCoverageLevel(value string) int {
	v := strings.TrimSpace(value)
	if v == "" {
		return -1
	}
	lower := strings.ToLower(v)
	if strings.Contains(lower, "not covered") || strings.Contains(lower, "no coverage") {
		return 0
	}
	v = strings.TrimSpace(strings.TrimSuffix(v, "%"))
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return -1
	}
	return int(f)
}

// ── maximums & deductibles ────────────────────────────────────────────────────

// knownAccumulatorNames maps Delta Dental counter IDs to human-readable names.
// The API type description is often generic ("Lifetime Individual Maximum")
// but the counter ID encodes the actual qualifier (OR = Orthodontic).
var knownAccumulatorNames = map[string]string{
	"CALYRMXN": "Calendar Annual Maximum",
	"LFTORMXN": "Lifetime Orthodontic Maximum",
	"CYLORMXN": "Calendar Ortho Maximum",
	"IMCYMXN":  "Calendar Implant Maximum",
	"CALTJMXN": "Calendar TMJ Maximum",
}

func applyMaximumsDeductibles(bundle *ddapi.PatientAPIBundle, el *eligibility.PatientEligibility) {
	if bundle.MaximumsDeductibles == nil {
		return
	}
	seen := map[string]bool{}
	for _, rec := range bundle.MaximumsDeductibles.MaximumsInfo {
		acc := buildAccumulatorFromDetails(rec.AmountInfo, rec.MaximumDetails, rec.ServicesAllowed, "maximum")
		if acc.AccumulatorID != "" && !seen[acc.AccumulatorID] {
			seen[acc.AccumulatorID] = true
			el.Accumulators = append(el.Accumulators, acc)
		}
	}
	for _, rec := range bundle.MaximumsDeductibles.DeductiblesInfo {
		acc := buildAccumulatorFromDetails(rec.AmountInfo, rec.DeductibleDetails, rec.ServicesAllowed, "deductible")
		if acc.AccumulatorID != "" && !seen[acc.AccumulatorID] {
			seen[acc.AccumulatorID] = true
			el.Accumulators = append(el.Accumulators, acc)
		}
	}
}

func buildAccumulatorFromDetails(amounts ddapi.MaxAmountInfo, details ddapi.MaxDetails, services []ddapi.MaxService, defaultKind string) eligibility.Accumulator {
	// Derive kind from the type description.
	lower := strings.ToLower(details.Type)
	kind := defaultKind
	if strings.Contains(lower, "deductible") {
		kind = "deductible"
	}

	// Derive accumulator period type from classification or type description.
	accType := "calendar"
	if strings.EqualFold(details.CalendarOrContractClassification, "LIFETIME") ||
		strings.Contains(lower, "lifetime") {
		accType = "lifetime"
	}

	// Use the API counter ID as the stable accumulator ID; fall back to a slug of the type.
	id := details.MaximumCounterID
	if id == "" {
		id = toSlug(details.Type)
	}
	if id == "" {
		id = fmt.Sprintf("%s-%.0f", kind, amounts.TotalAmount)
	}

	// Parse accumulator period dates when present.
	var period *eligibility.AccumulatorPeriod
	if details.AccumPeriodStartDate != "" && details.AccumPeriodEndDate != "" {
		layouts := []string{"2006-01-02", "01/02/2006", "1/2/2006"}
		var from, to time.Time
		for _, layout := range layouts {
			if t, err := time.Parse(layout, details.AccumPeriodStartDate); err == nil {
				from = t
				break
			}
		}
		for _, layout := range layouts {
			if t, err := time.Parse(layout, details.AccumPeriodEndDate); err == nil {
				to = t
				break
			}
		}
		if !from.IsZero() && !to.IsZero() {
			period = &eligibility.AccumulatorPeriod{From: from, To: to}
		}
	}

	// Build treatment type list from servicesAllowed, including the CDT codes
	// that count against each accumulator so downstream consumers can match
	// by procedure code rather than by treatment type name strings.
	var treatTypes []eligibility.AccumulatorTreatmentType
	for _, svc := range services {
		if svc.TreatmentTypeDescription == "" {
			continue
		}
		codes := make([]string, 0, len(svc.ProcedureCodesAllowed))
		for _, pc := range svc.ProcedureCodesAllowed {
			if c := strings.TrimSpace(pc.Code); c != "" {
				codes = append(codes, strings.ToUpper(c))
			}
		}
		treatTypes = append(treatTypes, eligibility.AccumulatorTreatmentType{
			Name:           svc.TreatmentTypeDescription,
			ProcedureCodes: codes,
		})
	}

	remaining := amounts.RemainingAmount
	if remaining == 0 && amounts.TotalAmount > 0 {
		// Compute remaining if API didn't supply it directly.
		remaining = amounts.TotalAmount - amounts.TotalUsedAmount
		if remaining < 0 {
			remaining = 0
		}
	}

	name := details.Type
	if mapped, ok := knownAccumulatorNames[id]; ok {
		name = mapped
	}
	name = stripScopeWords(name)

	return eligibility.Accumulator{
		AccumulatorID:             id,
		Name:                      name,
		Kind:                      kind,
		Type:                      accType,
		Scope:                     parseScopeFromName(details.Type),
		Period:                    period,
		Amount:                    amounts.TotalAmount,
		Used:                      amounts.TotalUsedAmount,
		Remaining:                 remaining,
		AccumulatorTreatmentTypes: treatTypes,
	}
}

func parseScopeFromName(name string) string {
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

// ── treatment history ─────────────────────────────────────────────────────────

func applyTreatmentHistory(bundle *ddapi.PatientAPIBundle, el *eligibility.PatientEligibility) {
	el.TreatmentHistory = make(map[string][]eligibility.TreatmentHistoryEntry)
	if bundle.TreatmentHistory == nil {
		return
	}
	for _, proc := range bundle.TreatmentHistory.Procedures {
		if proc.Code == "" {
			continue
		}
		el.TreatmentHistory[proc.Code] = append(el.TreatmentHistory[proc.Code], eligibility.TreatmentHistoryEntry{
			ServiceDate: proc.LastServiceDate,
		})
	}
}

// ── additional benefits → office summary ──────────────────────────────────────

func applyAdditionalBenefits(bundle *ddapi.PatientAPIBundle, el *eligibility.PatientEligibility) {
	if bundle.AdditionalBenefits == nil {
		return
	}
	for _, ab := range bundle.AdditionalBenefits.AdditionalBenefits {
		text := strings.TrimSpace(ab.Header)
		if body := strings.TrimSpace(ab.Text); body != "" {
			if text != "" {
				text += ": " + body
			} else {
				text = body
			}
		}
		if text == "" {
			continue
		}
		el.OfficeSummary = append(el.OfficeSummary, eligibility.OfficeSummaryNote{
			Tone: "info",
			Text: text,
		})
	}
}

// ── network helpers ───────────────────────────────────────────────────────────

func resolveNetworkTiers(defaultNetwork string) []eligibility.NetworkTier {
	lower := strings.ToLower(defaultNetwork)
	tiers := []eligibility.NetworkTier{
		{TierID: "in_network", DisplayName: "In Network", IsContracted: true},
		{TierID: "out_network", DisplayName: "Out of Network", IsContracted: false},
	}
	if strings.Contains(lower, "ppo") {
		tiers[0].DisplayName = defaultNetwork + " (In Network)"
	}
	return tiers
}

// ── small utilities ───────────────────────────────────────────────────────────

func normalizeSubscriberType(value string) string {
	lower := strings.ToLower(strings.TrimSpace(value))
	if strings.Contains(lower, "dep") || strings.Contains(lower, "child") || strings.Contains(lower, "spouse") {
		return "Dependent"
	}
	return "Subscriber"
}

func setProvision(el *eligibility.PatientEligibility, key, value string) {
	v := strings.TrimSpace(value)
	if v == "" {
		return
	}
	if el.Plan.Provisions == nil {
		el.Plan.Provisions = make(map[string]string)
	}
	el.Plan.Provisions[key] = v
}

func parseMoney(value string) float64 {
	v := strings.TrimSpace(value)
	v = strings.ReplaceAll(v, "$", "")
	v = strings.ReplaceAll(v, ",", "")
	f, _ := strconv.ParseFloat(v, 64)
	return f
}

func formatMoney(amount float64) string {
	return fmt.Sprintf("$%.2f", amount)
}

// stripScopeWords removes " Individual" and " Family" (case-insensitive) from
// accumulator names because the scope is already shown in the badge below the title.
func stripScopeWords(name string) string {
	upper := strings.ToUpper(name)
	for _, w := range []string{" INDIVIDUAL", " FAMILY"} {
		if idx := strings.Index(upper, w); idx >= 0 {
			name = strings.TrimSpace(name[:idx] + name[idx+len(w):])
			upper = strings.ToUpper(name)
		}
	}
	return name
}

func toSlug(value string) string {
	v := strings.ToLower(strings.TrimSpace(value))
	var b strings.Builder
	for _, r := range v {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			b.WriteRune(r)
		} else if r == ' ' || r == '-' || r == '_' {
			b.WriteByte('-')
		}
	}
	return strings.Trim(b.String(), "-")
}
