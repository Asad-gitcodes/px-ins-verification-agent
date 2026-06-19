package eligibility

import (
	"strconv"
	"strings"
	"time"

	elig "insurance-benefit-agent-go/internal/eligibility"
	gapi "insurance-benefit-agent-go/internal/payers/guardian/api"
)

func BuildEligibilityFromProbe(bundle *gapi.ProbeBundle) *elig.PatientEligibility {
	if bundle == nil || bundle.SelectedMember == nil {
		return nil
	}
	m := bundle.SelectedMember
	referenceDate := guardianReferenceDate(bundle)
	coverageEnd := guardianCoverageEndDate(bundle)
	active := strings.EqualFold(m.GRStatusCode, "A") && !dateBeforeOrEqual(coverageEnd, referenceDate)
	el := &elig.PatientEligibility{
		Patient: elig.PatientInfo{
			FullName:                 strings.TrimSpace(m.FirstName + " " + m.LastName),
			MemberType:               relationLabel(m.Relationship),
			DateOfBirth:              normalizeDate(m.DateOfBirth),
			MemberID:                 firstNonEmpty(bundle.Appointment.SubscriberID, employeeID(bundle)),
			GroupNumber:              m.GroupPolicyNumber,
			MemberEligibility:        statusLabel(m.GRStatusCode, active),
			EligibilityEffectiveDate: normalizeDate(firstNonEmpty(m.DentalCoverageEffectiveDate, m.EffectiveDate)),
			EligibilityEndDate:       normalizeDate(firstNonEmpty(coverageEnd, m.GRTerminationDate)),
			IsEligible:               active,
		},
		Plan: elig.PlanInfo{
			Carrier:    "Guardian",
			PlanName:   planName(bundle),
			GroupName:  strings.TrimSpace(m.GroupName),
			Provisions: map[string]string{},
		},
		Coverage: elig.Coverage{Categories: []elig.CoverageCategory{}},
		Metadata: elig.Metadata{
			EligibilityCheckedAt: bundle.RecordedAt,
			Source:               "Guardian Anytime API",
		},
	}
	if bundle.DentalPPO != nil {
		el.Plan.PlanDesign = bundle.DentalPPO.PlanTypeIndicator
		el.NetworkInfo = elig.NetworkInfo{Type: "ppo", DisplayName: "DG Preferred", Confidence: 80, Reason: "Guardian PPO VOB"}
		addPlanProvisions(el, bundle.DentalPPO)
		addAccumulators(el, bundle.DentalPPO)
		addCoverage(el, bundle.DentalPPO)
	}
	return el
}

func employeeID(bundle *gapi.ProbeBundle) string {
	if bundle.Member != nil {
		return bundle.Member.EmployeeMemberID
	}
	return ""
}

func planName(bundle *gapi.ProbeBundle) string {
	if bundle.DentalPPO != nil {
		if bundle.DentalPPO.Product.ProductName != "" && bundle.DentalPPO.Product.ProductType != "" {
			return bundle.DentalPPO.Product.ProductName + " " + bundle.DentalPPO.Product.ProductType
		}
		info := firstPPOBenefit(bundle.DentalPPO).BenefitInformation
		if info.BenefitPlanType != "" {
			return info.BenefitPlanType
		}
	}
	return "Guardian Dental"
}

func addPlanProvisions(el *elig.PatientEligibility, dental *gapi.DentalPPOResponse) {
	info := firstPPOBenefit(dental).BenefitInformation
	provisions := el.Plan.Provisions
	provisions["Benefit Plan Type"] = info.BenefitPlanType
	provisions["Benefit Period"] = strings.TrimSpace(info.BenefitPeriodEffectiveDate + " - " + info.BenefitPeriodEndDate)
	provisions["Network Config Code"] = dental.NetworkConfigCode
	if dental.MaxRollover.MaximumRolloverAmount != "" {
		provisions["Maximum Rollover"] = dental.MaxRollover.MaximumRolloverAmount
	}
	if dental.MaxRollover.MaxrolloverAmount != "" {
		provisions["Rollover Account"] = dental.MaxRollover.MaxrolloverAmount
	}
	for _, limit := range dental.AgeLimit {
		if limit.BenefitCategory != "" && limit.Age != "" {
			provisions["Age Limit - "+limit.BenefitCategory] = limit.Age
		}
	}
}

func addAccumulators(el *elig.PatientEligibility, dental *gapi.DentalPPOResponse) {
	for _, max := range firstPPOBenefit(dental).PlanMaximum {
		amount := parseMoney(max.Amount)
		if amount == 0 && !strings.Contains(max.Amount, "$0") {
			continue
		}
		kind := "maximum"
		name := strings.TrimSpace(max.PlanMaximumForBenefit + " " + max.TimeQualifier + " " + max.NetworkName)
		acc := elig.Accumulator{
			AccumulatorID: strings.ToLower(strings.ReplaceAll(name, " ", "-")),
			Name:          name,
			Kind:          kind,
			Type:          periodType(max.TimeQualifier),
			Scope:         "individual",
			Amount:        amount,
		}
		if strings.Contains(strings.ToLower(max.TimeQualifier), "met-to-date") {
			acc.Used = amount
		}
		el.Accumulators = append(el.Accumulators, acc)
	}
	for _, ded := range firstPPOBenefit(dental).Deductible {
		amount := parseMoney(ded.Amount)
		acc := elig.Accumulator{
			AccumulatorID: strings.ToLower(strings.ReplaceAll("deductible "+ded.CoverageTier+" "+ded.NetworkName+" "+ded.DeductiblePeriod, " ", "-")),
			Name:          strings.TrimSpace(ded.CoverageTier + " " + ded.DeductiblePeriod + " " + ded.NetworkName),
			Kind:          "deductible",
			Type:          "calendar",
			Scope:         scopeFromTier(ded.CoverageTier),
			Amount:        amount,
		}
		if strings.EqualFold(ded.DeductiblePeriod, "Met-To-Date") {
			acc.Used = amount
		}
		el.Accumulators = append(el.Accumulators, acc)
	}
}

func addCoverage(el *elig.PatientEligibility, dental *gapi.DentalPPOResponse) {
	categories := map[string][]elig.CoverageService{}
	for _, option := range firstPPOBenefit(dental).PlanOption {
		category := "Other"
		if len(option.Category) > 0 && option.Category[0].CategoryType != "" {
			category = option.Category[0].CategoryType
		} else if option.DentalService != "" {
			category = option.DentalService
		}
		service := elig.CoverageService{
			Description:        option.DentalService,
			Limitations:        strings.Join(option.Message, " "),
			AgeLimits:          "",
			DeductibleExempted: hasDeductibleWaived(dental, category),
		}
		if len(option.Coinsurance) > 0 {
			service.CoveragePercent = parsePercent(option.Coinsurance[0].CoinsuranceAmount)
		}
		if option.LastVisitDate != "" {
			service.Limitations = strings.TrimSpace(service.Limitations + " Last visit: " + option.LastVisitDate)
		}
		categories[category] = append(categories[category], service)
	}
	for name, services := range categories {
		el.Coverage.Categories = append(el.Coverage.Categories, elig.CoverageCategory{Name: name, Services: services})
	}
}

func firstPPOBenefit(dental *gapi.DentalPPOResponse) gapi.PPOBenefit {
	if dental != nil && len(dental.PPOBenefit) > 0 {
		return dental.PPOBenefit[0]
	}
	return gapi.PPOBenefit{}
}

func hasDeductibleWaived(dental *gapi.DentalPPOResponse, category string) bool {
	for _, row := range firstPPOBenefit(dental).BenefitInformation.ServiceCategoryEffectiveDate {
		if strings.EqualFold(row.DentalServiceCategory, category) {
			return strings.EqualFold(row.InNetworkDeductibleWaived, "Y")
		}
	}
	return false
}

func relationLabel(code string) string {
	switch strings.ToUpper(strings.TrimSpace(code)) {
	case "M":
		return "Subscriber"
	case "S":
		return "Dependent - Spouse"
	case "D":
		return "Dependent - Child"
	default:
		return "Dependent"
	}
}

func statusLabel(code string, active bool) string {
	if active {
		return "Active"
	}
	if strings.EqualFold(code, "A") {
		return "Inactive"
	}
	if strings.TrimSpace(code) == "" {
		return "Unknown"
	}
	return code
}

func guardianReferenceDate(bundle *gapi.ProbeBundle) string {
	if bundle == nil {
		return ""
	}
	return firstNonEmpty(bundle.Appointment.AppointmentDate, bundle.RecordedAt)
}

func guardianCoverageEndDate(bundle *gapi.ProbeBundle) string {
	if bundle == nil || bundle.SelectedMember == nil {
		return ""
	}
	var memberTermination string
	if bundle.Member != nil {
		memberTermination = bundle.Member.TerminationDate
	}
	return firstNonEmpty(bundle.SelectedMember.MemberCoverageTermDate, memberTermination)
}

func dateBeforeOrEqual(value, reference string) bool {
	date, ok := parseGuardianDate(value)
	if !ok {
		return false
	}
	ref, ok := parseGuardianDate(reference)
	if !ok {
		ref = time.Now()
	}
	return !date.After(ref)
}

func normalizeDate(value string) string {
	value = strings.TrimSpace(value)
	if t, ok := parseGuardianDate(value); ok {
		return t.Format("2006-01-02")
	}
	return value
}

func parseGuardianDate(value string) (time.Time, bool) {
	value = strings.TrimSpace(value)
	if len(value) >= len("2006-01-02T15:04:05Z") {
		if t, err := time.Parse(time.RFC3339, value); err == nil {
			return t, true
		}
	}
	for _, layout := range []string{"01/02/2006", "01-02-2006", "2006-01-02", "01/02/06"} {
		if t, err := time.Parse(layout, value); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func parseMoney(value string) float64 {
	value = strings.TrimSpace(value)
	value = strings.TrimPrefix(value, "$")
	value = strings.ReplaceAll(value, ",", "")
	f, _ := strconv.ParseFloat(value, 64)
	return f
}

func parsePercent(value string) int {
	value = strings.TrimSuffix(strings.TrimSpace(value), "%")
	i, _ := strconv.Atoi(value)
	return i
}

func periodType(value string) string {
	if strings.Contains(strings.ToLower(value), "lifetime") {
		return "lifetime"
	}
	return "calendar"
}

func scopeFromTier(value string) string {
	if strings.Contains(strings.ToLower(value), "family") {
		return "family"
	}
	return "individual"
}
