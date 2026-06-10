package eligibility

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"insurance-benefit-agent-go/internal/eligibility"
	emapi "insurance-benefit-agent-go/internal/payers/emblemhealth/api"
)

func BuildEligibilityFromProbe(bundle *emapi.ProbeBundle) *eligibility.PatientEligibility {
	if bundle == nil || bundle.Record == nil {
		return nil
	}
	record := bundle.Record
	fullName := strings.TrimSpace(record.MemberFirstName + " " + record.MemberLastName)
	if fullName == "" {
		fullName = strings.TrimSpace(record.MemberCombName)
	}

	el := &eligibility.PatientEligibility{
		Patient: eligibility.PatientInfo{
			FullName:                 fullName,
			DateOfBirth:              normalizeDate(record.BirthDate),
			MemberID:                 firstNonEmpty(record.MemberID, record.MemberAltID, record.AltMemberID),
			MemberType:               "Subscriber",
			MemberEligibility:        strings.TrimSpace(record.Status),
			EligibilityEffectiveDate: normalizeDate(record.EligibilityEffectiveDate),
			EligibilityEndDate:       normalizeDate(record.EligibilityTerminationDate),
			IsEligible:               strings.EqualFold(strings.TrimSpace(record.Status), "Active"),
		},
		Plan: eligibility.PlanInfo{
			Carrier:    firstNonEmpty(record.OriginalBrand, "EmblemHealth"),
			PlanName:   strings.TrimSpace(record.ProductType),
			PlanDesign: strings.TrimSpace(record.PlanType),
			Provisions: map[string]string{},
		},
		Coverage:      eligibility.Coverage{Categories: []eligibility.CoverageCategory{}},
		NetworkTiers:  []eligibility.NetworkTier{},
		NetworkMatrix: []eligibility.NetworkMatrixRow{},
		Accumulators:  []eligibility.Accumulator{},
		OfficeSummary: []eligibility.OfficeSummaryNote{},
		Metadata: eligibility.Metadata{
			EligibilityCheckedAt: time.Now().UTC().Format(time.RFC3339),
			Source:               "EmblemHealthApexRemote",
		},
	}

	setProvision(el, "Subscriber ID", record.SubscriberID)
	setProvision(el, "Plan Code", record.PlanCode)
	setProvision(el, "Plan Category", record.PlanCategoryNum)
	setProvision(el, "Coverage Type", record.CoverageType)
	setProvision(el, "Product Type", record.ProductType)
	setProvision(el, "Original Brand", record.OriginalBrand)
	applyDetailData(bundle, el)
	return el
}

func applyDetailData(bundle *emapi.ProbeBundle, el *eligibility.PatientEligibility) {
	if bundle == nil || el == nil {
		return
	}
	detailData := bundle.DetailData
	if details, ok := parseDetailPayload[memberDetailsPayload](detailData["Member_DetailsInformation"]); ok {
		info := details.IPResult.Response
		setProvision(el, "Group ID", info.GroupID)
		setProvision(el, "Group Name", info.GroupName)
		setProvision(el, "Product Description", info.ProductDescription)
		setProvision(el, "Network", info.NetworkProviderInformation.Item.NetworkName)
		if info.GroupName != "" {
			el.Plan.GroupName = info.GroupName
		}
		if info.ProductDescription != "" {
			el.Plan.PlanName = info.ProductDescription
		}
		if info.NetworkProviderInformation.Item.NetworkName != "" {
			el.NetworkInfo = eligibility.NetworkInfo{
				Type:        strings.TrimSpace(info.NetworkProviderInformation.Item.NetworkCode),
				DisplayName: strings.TrimSpace(info.NetworkProviderInformation.Item.NetworkName),
				Confidence:  80,
				Reason:      "EmblemHealth member detail network",
			}
		}
	}
	limits := parseLimitationMap(detailData["MemberDetails_DentalLimitation"])
	coverageByCategory := map[string]int{}
	if out, ok := parseDetailPayload[dentalNetworkPayload](detailData["MemberDetails_DentalOutNetwork"]); ok {
		applyNetworkBenefits(el, out, "out", "Out Network", coverageByCategory)
	}
	if in_, ok := parseDetailPayload[dentalNetworkPayload](detailData["MemberDetails_DentalInNetwork"]); ok {
		applyNetworkBenefits(el, in_, "in", "In Network", coverageByCategory)
	}
	// Deduplicate accumulators by ID — in-network and out-network often report the same combined max
	seen := map[string]bool{}
	var deduped []eligibility.Accumulator
	for _, acc := range el.Accumulators {
		if !seen[acc.AccumulatorID] {
			seen[acc.AccumulatorID] = true
			deduped = append(deduped, acc)
		}
	}
	el.Accumulators = deduped
	if acc, ok := parseDetailPayload[benefitAccumulatorPayload](detailData["MemberDetails_BenefitAccumulator"]); ok {
		applyBenefitAccumulator(el, acc)
	}
	if hist, ok := parseDetailPayload[toothHistoryPayload](detailData["MemberDetails_ToothHistory"]); ok {
		applyToothHistory(el, hist)
	}
	addCoverageForAppointmentCodes(el, appointmentCodes(bundle), coverageByCategory, limits)
}

type apexEnvelope struct {
	StatusCode int             `json:"statusCode"`
	RawResult  json.RawMessage `json:"result"`
}

func parseDetailPayload[T any](raw json.RawMessage) (T, bool) {
	var zero T
	if len(raw) == 0 {
		return zero, false
	}
	var responses []apexEnvelope
	if err := json.Unmarshal(raw, &responses); err != nil || len(responses) == 0 || responses[0].StatusCode < 200 || responses[0].StatusCode >= 300 {
		return zero, false
	}
	// doGenericInvoke: result is a JSON string
	// doAsyncInvoke:   result is {"result":"...","responseId":"..."}
	resultStr := ""
	if err := json.Unmarshal(responses[0].RawResult, &resultStr); err != nil {
		var asyncObj struct {
			Result string `json:"result"`
		}
		if err2 := json.Unmarshal(responses[0].RawResult, &asyncObj); err2 != nil || asyncObj.Result == "" {
			return zero, false
		}
		resultStr = asyncObj.Result
	}
	var payload T
	if err := json.Unmarshal([]byte(resultStr), &payload); err != nil {
		return zero, false
	}
	return payload, true
}

type memberDetailsPayload struct {
	IPResult struct {
		Response struct {
			GroupID                    string `json:"groupId"`
			GroupName                  string `json:"groupName"`
			ProductDescription         string `json:"productDescription"`
			NetworkProviderInformation struct {
				Item struct {
					NetworkCode string `json:"networkcode"`
					NetworkName string `json:"networkname"`
				} `json:"Item"`
			} `json:"networkProviderInformation"`
		} `json:"Response"`
	} `json:"IPResult"`
}

type dentalNetworkPayload struct {
	IPResult struct {
		CoverageBenefitsResponse []struct {
			BenefitDetails struct {
				Accumulator struct {
					CoverageLevel      string `json:"coverageLevel"`
					AnnualMaxUsed      string `json:"annualMaxUsed"`
					AnnualMaxRemaining string `json:"annualMaxRemaining"`
					AnnualMaxAmount    string `json:"annualMaxAmount"`
				} `json:"accumulator"`
				Benefits []struct {
					ServiceTypeDescription string `json:"serviceTypeDescription"`
					Coinsurance            string `json:"coinsurance"`
				} `json:"benefits"`
			} `json:"benefitDetails"`
		} `json:"coverageBenefitsResponse"`
		Ortho []struct {
			ServiceTypeDescription   string `json:"serviceTypeDescription"`
			MemberCoinsurance        string `json:"memberCoinsurance"`
			LifeTimeMaximumUsed      string `json:"lifeTimeMaximumUsed"`
			LifeTimeMaximumRemaining string `json:"lifeTimeMaximumRemaining"`
			LifeTimeMaximum          string `json:"lifeTimeMaximum"`
		} `json:"Ortho"`
		Deductible []struct {
			ServiceTypeDescription string `json:"serviceTypeDescription"`
			TotalDeductible        string `json:"totalDeductible"`
			DeductibleRemaining    string `json:"deductibleRemaining"`
			DeductibleMet          string `json:"deductibleMet"`
		} `json:"Deductible"`
	} `json:"IPResult"`
}

type benefitAccumulatorPayload struct {
	IPResult struct {
		BenefitAccumulatorInfo struct {
			TotalAmount     string `json:"totalamount"`
			Period          string `json:"period"`
			Description     string `json:"description"`
			AmountRemaining string `json:"amountremaining"`
			AmountMet       string `json:"amountmet"`
			CoverageLevel   string `json:"coverageLevel"`
		} `json:"BenefitAccumulatorInfo"`
	} `json:"IPResult"`
}

type toothHistoryPayload struct {
	IPResult struct {
		MemberToothHistory []struct {
			ToothBegin               string `json:"toothBegin"`
			ServiceDate              string `json:"serviceDate"`
			ProcedureCodeDescription string `json:"procedureCodeDescription"`
			ProcedureCode            string `json:"procedureCode"`
		} `json:"MemberToothHistory"`
	} `json:"IPResult"`
}

type limitationPayload struct {
	IPResult struct {
		CoverageInfo []struct {
			Description string `json:"agelimitfrequencydescription"`
			Name        string `json:"agelimitfrequency"`
		} `json:"coverageInfo"`
	} `json:"IPResult"`
}

func applyNetworkBenefits(el *eligibility.PatientEligibility, payload dentalNetworkPayload, tierID, tierName string, coverageByCategory map[string]int) {
	el.NetworkTiers = append(el.NetworkTiers, eligibility.NetworkTier{TierID: tierID, DisplayName: tierName, IsContracted: false})
	for _, row := range payload.IPResult.CoverageBenefitsResponse {
		acc := row.BenefitDetails.Accumulator
		if acc.AnnualMaxAmount != "" {
			el.Accumulators = append(el.Accumulators, eligibility.Accumulator{
				AccumulatorID: "emblem-annual-maximum",
				Name:          "Annual Maximum",
				Kind:          "maximum",
				Type:          "calendar",
				Scope:         strings.ToLower(acc.CoverageLevel),
				Amount:        parseAmount(acc.AnnualMaxAmount),
				Used:          parseAmount(acc.AnnualMaxUsed),
				Remaining:     parseAmount(acc.AnnualMaxRemaining),
			})
		}
		for _, benefit := range row.BenefitDetails.Benefits {
			category := categoryFromBenefit(benefit.ServiceTypeDescription)
			pct := parsePercent(benefit.Coinsurance)
			if category != "" {
				coverageByCategory[category] = pct
				el.NetworkMatrix = append(el.NetworkMatrix, eligibility.NetworkMatrixRow{
					Name:   category,
					Values: map[string]string{tierID: fmt.Sprintf("%d%%", pct)},
				})
			}
		}
	}
	for _, ortho := range payload.IPResult.Ortho {
		pct := parsePercent(ortho.MemberCoinsurance)
		coverageByCategory["Orthodontics"] = pct
		el.NetworkMatrix = append(el.NetworkMatrix, eligibility.NetworkMatrixRow{
			Name:   "Orthodontics",
			Values: map[string]string{tierID: fmt.Sprintf("%d%%", pct)},
		})
		if ortho.LifeTimeMaximum != "" {
			el.Accumulators = append(el.Accumulators, eligibility.Accumulator{
				AccumulatorID: "emblem-ortho-lifetime-maximum",
				Name:          "Orthodontic Lifetime Maximum",
				Kind:          "maximum",
				Type:          "lifetime",
				Scope:         "individual",
				Amount:        parseAmount(ortho.LifeTimeMaximum),
				Used:          parseAmount(ortho.LifeTimeMaximumUsed),
				Remaining:     parseAmount(ortho.LifeTimeMaximumRemaining),
			})
		}
	}
	for _, ded := range payload.IPResult.Deductible {
		amount := parseAmount(ded.TotalDeductible)
		if amount == 0 && parseAmount(ded.DeductibleMet) == 0 && parseAmount(ded.DeductibleRemaining) == 0 {
			continue
		}
		el.Accumulators = append(el.Accumulators, eligibility.Accumulator{
			AccumulatorID: strings.ToLower(strings.ReplaceAll("emblem "+ded.ServiceTypeDescription, " ", "-")),
			Name:          ded.ServiceTypeDescription,
			Kind:          "deductible",
			Type:          "calendar",
			Scope:         scopeFromText(ded.ServiceTypeDescription),
			Amount:        amount,
			Used:          parseAmount(ded.DeductibleMet),
			Remaining:     parseAmount(ded.DeductibleRemaining),
		})
	}
}

func applyBenefitAccumulator(el *eligibility.PatientEligibility, payload benefitAccumulatorPayload) {
	info := payload.IPResult.BenefitAccumulatorInfo
	if info.TotalAmount == "" {
		return
	}
	el.Accumulators = append(el.Accumulators, eligibility.Accumulator{
		AccumulatorID: "emblem-benefit-accumulator",
		Name:          firstNonEmpty(info.Description, "Benefit Accumulator"),
		Kind:          "maximum",
		Type:          accumulatorPeriod(info.Period),
		Scope:         strings.ToLower(info.CoverageLevel),
		Amount:        parseAmount(info.TotalAmount),
		Used:          parseAmount(info.AmountMet),
		Remaining:     parseAmount(info.AmountRemaining),
	})
}

func applyToothHistory(el *eligibility.PatientEligibility, payload toothHistoryPayload) {
	if el.TreatmentHistory == nil {
		el.TreatmentHistory = map[string][]eligibility.TreatmentHistoryEntry{}
	}
	for _, row := range payload.IPResult.MemberToothHistory {
		code := normalizeCode(row.ProcedureCode)
		if code == "" {
			continue
		}
		el.TreatmentHistory[code] = append(el.TreatmentHistory[code], eligibility.TreatmentHistoryEntry{
			ServiceDate:      normalizeDate(row.ServiceDate),
			ToothCode:        row.ToothBegin,
			ToothDescription: row.ProcedureCodeDescription,
		})
	}
}

func addCoverageForAppointmentCodes(el *eligibility.PatientEligibility, codes []string, coverageByCategory map[string]int, limits map[string]string) {
	byCategory := map[string][]eligibility.CoverageService{}
	missingCoverage := false
	for _, code := range codes {
		code = normalizeCode(code)
		if code == "" {
			continue
		}
		category := categoryForCDT(code)
		pct, ok := coverageByCategory[category]
		if !ok {
			pct = -1
			missingCoverage = true
		}
		byCategory[category] = append(byCategory[category], eligibility.CoverageService{
			Code:            code,
			Description:     descriptionForCDT(code),
			CoveragePercent: pct,
			Limitations:     limitationForCode(code, limits),
		})
	}
	for category, services := range byCategory {
		el.Coverage.Categories = append(el.Coverage.Categories, eligibility.CoverageCategory{Name: category, Services: services})
	}
	if missingCoverage {
		el.OfficeSummary = append(el.OfficeSummary, eligibility.OfficeSummaryNote{
			Tone: "info",
			Text: "EmblemHealth did not return procedure-level coverage for one or more requested CDT codes; those procedures are marked unknown, not denied.",
		})
	}
}

func parseLimitationMap(raw json.RawMessage) map[string]string {
	out := map[string]string{}
	payload, ok := parseDetailPayload[limitationPayload](raw)
	if !ok {
		return out
	}
	for _, row := range payload.IPResult.CoverageInfo {
		name := strings.TrimSpace(row.Name)
		desc := strings.TrimSpace(row.Description)
		if name != "" && desc != "" {
			out[name] = desc
		}
	}
	return out
}

func normalizeDate(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	layouts := []struct{ in, out string }{
		{"01/02/2006", "2006-01-02"},
		{"1/2/2006", "2006-01-02"},
		{"2006-01-02", "2006-01-02"},
	}
	for _, layout := range layouts {
		if parsed, err := time.Parse(layout.in, value); err == nil {
			return parsed.Format(layout.out)
		}
	}
	return value
}

func setProvision(el *eligibility.PatientEligibility, key, value string) {
	value = strings.TrimSpace(value)
	if value == "" {
		return
	}
	if el.Plan.Provisions == nil {
		el.Plan.Provisions = map[string]string{}
	}
	el.Plan.Provisions[key] = value
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" {
			return value
		}
	}
	return ""
}

func appointmentCodes(bundle *emapi.ProbeBundle) []string {
	if bundle == nil {
		return nil
	}
	raw := ""
	if appt, ok := bundle.Appointment.(map[string]any); ok {
		raw, _ = appt["treatmentPlanProcCodes"].(string)
	} else {
		data, _ := json.Marshal(bundle.Appointment)
		var appt struct {
			TreatmentPlanProcCodes string `json:"treatmentPlanProcCodes"`
		}
		_ = json.Unmarshal(data, &appt)
		raw = appt.TreatmentPlanProcCodes
	}
	parts := strings.FieldsFunc(raw, func(r rune) bool {
		return r == ',' || r == ';' || r == '\n' || r == '\r' || r == '\t' || r == ' '
	})
	var codes []string
	seen := map[string]bool{}
	for _, part := range parts {
		code := normalizeCode(part)
		if code != "" && !seen[code] {
			seen[code] = true
			codes = append(codes, code)
		}
	}
	return codes
}

func normalizeCode(value string) string {
	value = strings.ToUpper(strings.TrimSpace(value))
	value = strings.Trim(value, ".,;")
	if len(value) >= 5 && value[0] == 'D' {
		return value[:5]
	}
	return value
}

func parseAmount(value string) float64 {
	value = strings.TrimSpace(value)
	value = strings.TrimPrefix(value, "$")
	value = strings.ReplaceAll(value, ",", "")
	f, _ := strconv.ParseFloat(value, 64)
	return f
}

func parsePercent(value string) int {
	value = strings.TrimSuffix(strings.TrimSpace(value), "%")
	f, _ := strconv.ParseFloat(value, 64)
	return int(f + 0.5)
}

func categoryFromBenefit(value string) string {
	lower := strings.ToLower(value)
	switch {
	case strings.Contains(lower, "preventive"):
		return "Preventive"
	case strings.Contains(lower, "basic"):
		return "Basic"
	case strings.Contains(lower, "major"):
		return "Major"
	case strings.Contains(lower, "ortho"):
		return "Orthodontics"
	default:
		return ""
	}
}

func categoryForCDT(code string) string {
	if len(code) < 5 || code[0] != 'D' {
		return "Other"
	}
	n, _ := strconv.Atoi(code[1:5])
	switch {
	case n >= 100 && n <= 999:
		return "Preventive" // EmblemHealth covers diagnostics under Preventive, no separate Diagnostic bucket
	case n >= 1000 && n <= 1999:
		return "Preventive"
	case n >= 2000 && n <= 2999:
		return "Basic"
	case n >= 3000 && n <= 3999:
		return "Basic"
	case n >= 4000 && n <= 4999:
		return "Basic"
	case n >= 5000 && n <= 6999:
		return "Major"
	case n >= 7000 && n <= 7999:
		return "Basic"
	case n >= 8000 && n <= 8999:
		return "Orthodontics"
	default:
		return "Major"
	}
}

func descriptionForCDT(code string) string {
	switch code {
	case "D0120":
		return "Periodic oral evaluation"
	case "D0220":
		return "Intraoral periapical first film"
	case "D0230":
		return "Intraoral periapical each additional film"
	case "D0274":
		return "Bitewings four films"
	case "D1110":
		return "Adult prophylaxis"
	case "D2740":
		return "Crown - porcelain/ceramic"
	case "D2954":
		return "Prefabricated post and core"
	case "D4341":
		return "Periodontal scaling and root planing"
	case "D6311":
		return "Fixed bilateral space maintainer"
	default:
		return ""
	}
}

func limitationForCode(code string, limits map[string]string) string {
	var parts []string
	add := func(key string) {
		if value := strings.TrimSpace(limits[key]); value != "" {
			parts = append(parts, key+": "+value)
		}
	}
	switch code {
	case "D0120", "D0140", "D0150":
		add("Oral Exam Frequency")
	case "D0210":
		add("Full Mouth Xray Frequency")
	case "D0220", "D0230":
		add("Full Mouth Xray Frequency")
	case "D0274":
		add("Bitewing Xray Frequency")
	case "D0330":
		add("Panoramic Xray Frequency")
	case "D1110":
		add("Prophylaxis Max Age")
	case "D2740", "D2954":
		add("Std. Crown Frequency")
	case "D4341", "D4342":
		add("Root Plane Frequency")
	case "D6311":
		add("Space Main Age")
	}
	return strings.Join(parts, "; ")
}

func accumulatorPeriod(value string) string {
	if strings.Contains(strings.ToLower(value), "life") {
		return "lifetime"
	}
	return "calendar"
}

func scopeFromText(value string) string {
	if strings.Contains(strings.ToLower(value), "family") {
		return "family"
	}
	return "individual"
}
