package api

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"insurance-benefit-agent-go/internal/models"
	"insurance-benefit-agent-go/internal/payers/dentaquest/eligibility"
	"insurance-benefit-agent-go/internal/scrapeutil"
)

var (
	normalizeSpace = scrapeutil.NormalizeSpace
	parseMoney     = scrapeutil.ParseMoney
	toSlug         = scrapeutil.ToSlug
	asStringMap    = scrapeutil.AsStringMap
	asSlice        = scrapeutil.AsSlice
	anyStr         = scrapeutil.AnyStr
)

func BuildEligibilityFromProbeBundle(bundle *PatientAPIBundle) *eligibility.PatientEligibility {
	if bundle == nil {
		return nil
	}

	el := buildPatientEligibility(bundle.Appointment)
	applySearchResultSummary(bundle, el)
	applyMemberInfoSummary(bundle, el)
	applyEnrollmentSummary(bundle, el)
	applyPlanBenefitSummary(bundle, el)
	applyPlanBenefitSummaryNetworkMatrix(bundle, el)
	applyMaximumDeductible(bundle, el)
	el.TreatmentHistory = buildTreatmentHistory(bundle.Clinical, 100)
	if el.TreatmentHistory == nil {
		el.TreatmentHistory = make(map[string][]eligibility.TreatmentHistoryEntry)
	}
	el.Metadata = eligibility.Metadata{
		EligibilityCheckedAt: time.Now().UTC().Format(time.RFC3339),
		Source:               "DentaQuestAPIProbe",
	}
	return el
}

func buildPatientEligibility(appointment models.Appointment) *eligibility.PatientEligibility {
	memberType := "Subscriber"
	if strings.EqualFold(appointment.Relationship, "dependent") {
		memberType = "Dependent"
	}

	fullName := normalizeSpace(fmt.Sprintf("%s %s", appointment.FName, appointment.LName))
	return &eligibility.PatientEligibility{
		Patient: eligibility.PatientInfo{
			FullName:    fullName,
			MemberType:  memberType,
			DateOfBirth: appointment.DOB,
			MemberID:    appointment.SubscriberID,
			IsEligible:  true,
		},
		Plan: eligibility.PlanInfo{
			Carrier:    "DentaQuest",
			Provisions: make(map[string]string),
		},
		Coverage:      eligibility.Coverage{Categories: []eligibility.CoverageCategory{}},
		NetworkTiers:  []eligibility.NetworkTier{},
		NetworkMatrix: []eligibility.NetworkMatrixRow{},
		Accumulators:  []eligibility.Accumulator{},
		OfficeSummary: []eligibility.OfficeSummaryNote{},
	}
}

func applySearchResultSummary(bundle *PatientAPIBundle, el *eligibility.PatientEligibility) {
	if el == nil {
		return
	}
	el.Patient.FullName = firstProbeNonEmpty(normalizeSpace(bundle.SearchResult.FirstName+" "+bundle.SearchResult.LastName), el.Patient.FullName)
	el.Patient.DateOfBirth = firstProbeNonEmpty(bundle.SearchResult.DateOfBirth, el.Patient.DateOfBirth)
	el.Patient.MemberID = firstProbeNonEmpty(bundle.SearchResult.MemberID, el.Patient.MemberID)
}

func applyMemberInfoSummary(bundle *PatientAPIBundle, el *eligibility.PatientEligibility) {
	payload := asStringMap(bundle.MemberInfo)
	if payload == nil {
		return
	}

	el.Patient.FullName = firstProbeNonEmpty(anyStr(payload, "memberName"), el.Patient.FullName)
	el.Patient.DateOfBirth = firstProbeNonEmpty(anyStr(payload, "memberDateOfBirth"), el.Patient.DateOfBirth)
	el.Patient.MemberID = firstProbeNonEmpty(anyStr(payload, "memberId"), el.Patient.MemberID)
	el.Patient.MemberEligibility = firstProbeNonEmpty(anyStr(payload, "memberEligibilityStatus"), el.Patient.MemberEligibility)
	el.Patient.IsEligible = interpretEligibilityStatus(anyStr(payload, "memberEligibilityStatus"), el.Patient.IsEligible)
	el.Patient.GroupNumber = firstProbeNonEmpty(anyStr(payload, "unparsedSubGroupNumber"), el.Patient.GroupNumber)

	if level := anyStr(payload, "memberCoverageLevel"); level != "" {
		lower := strings.ToLower(level)
		if strings.Contains(lower, "employee") && !regexp.MustCompile(`spouse|child|dependent`).MatchString(lower) {
			el.Patient.MemberType = "Subscriber"
		} else {
			el.Patient.MemberType = "Dependent"
		}
	}

	el.Plan.PlanName = firstProbeNonEmpty(anyStr(payload, "memberPlanName"), el.Plan.PlanName)
	el.Plan.GroupName = firstProbeNonEmpty(anyStr(payload, "unparsedParentGroupName"), el.Plan.GroupName)
	el.Plan.PlanDesign = firstProbeNonEmpty(anyStr(payload, "productNew"), el.Plan.PlanDesign)

	el.NetworkInfo.Type = firstProbeNonEmpty(anyStr(payload, "networkNew"), anyStr(payload, "networkName"), el.NetworkInfo.Type)
	el.NetworkInfo.DisplayName = firstProbeNonEmpty(anyStr(payload, "networkContractNew"), anyStr(payload, "networkName"), el.NetworkInfo.DisplayName)
	if el.NetworkInfo.Type != "" || el.NetworkInfo.DisplayName != "" {
		el.NetworkInfo.Confidence = 1
		el.NetworkInfo.Reason = "Parsed from member info payload"
	}

	setProvision(el, "Coverage level", anyStr(payload, "memberCoverageLevel"))
	setProvision(el, "Coverage type", anyStr(payload, "memberCoverageType"))
	setProvision(el, "Network", anyStr(payload, "networkName"))
	setProvision(el, "Network contract", anyStr(payload, "networkContractNew"))
	setProvision(el, "Practitioner", anyStr(payload, "practitionerName"))
	setProvision(el, "Location address", anyStr(payload, "locationAddress"))
	setProvision(el, "Parent group name", anyStr(payload, "unparsedParentGroupName"))
	setProvision(el, "Parent group number", anyStr(payload, "unparsedParentGroupNumber"))
	setProvision(el, "Sub group name", anyStr(payload, "unparsedSubGroupName"))
	setProvision(el, "Sub group number", anyStr(payload, "unparsedSubGroupNumber"))
	setProvision(el, "Product", anyStr(payload, "productNew"))
	setProvision(el, "Product category", anyStr(payload, "productCategory"))
}

func applyEnrollmentSummary(bundle *PatientAPIBundle, el *eligibility.PatientEligibility) {
	current := pickCurrentEnrollmentRecord(asSlice(bundle.Enrollment))
	if current == nil {
		return
	}

	el.Patient.EligibilityEffectiveDate = firstProbeNonEmpty(anyStr(current, "effectiveDate"), el.Patient.EligibilityEffectiveDate)
	el.Patient.EligibilityEndDate = firstProbeNonEmpty(normalizeEndDate(anyStr(current, "terminationDate")), el.Patient.EligibilityEndDate)
	el.Plan.PlanName = firstProbeNonEmpty(anyStr(current, "planName"), el.Plan.PlanName)
	el.Plan.GroupName = firstProbeNonEmpty(anyStr(current, "unparsedParentGroupName"), el.Plan.GroupName)
	el.Plan.PlanDesign = firstProbeNonEmpty(anyStr(current, "productNew"), el.Plan.PlanDesign)

	setProvision(el, "Enrollment status", anyStr(current, "status"))
	setProvision(el, "Enrollment effective date", anyStr(current, "effectiveDate"))
	setProvision(el, "Enrollment termination date", anyStr(current, "terminationDate"))
	setProvision(el, "Parent group name", anyStr(current, "unparsedParentGroupName"))
	setProvision(el, "Parent group number", anyStr(current, "unparsedParentGroupNumber"))
	setProvision(el, "Sub group name", anyStr(current, "unparsedSubGroupName"))
	setProvision(el, "Sub group number", anyStr(current, "unparsedSubGroupNumber"))
	setProvision(el, "Product", anyStr(current, "productNew"))
	setProvision(el, "Product category", anyStr(current, "productCategory"))
}

func applyPlanBenefitSummary(bundle *PatientAPIBundle, el *eligibility.PatientEligibility) {
	payload := asStringMap(bundle.Benefit)
	if payload == nil {
		return
	}

	el.Plan.PlanName = firstProbeNonEmpty(anyStr(payload, "planName"), el.Plan.PlanName)
	setProvision(el, "Benefit summary plan name", anyStr(payload, "planName"))
	setProvision(el, "Benefit summary plan ID", anyStr(payload, "planId"))
}

var dentaquestNetworkTiers = []eligibility.NetworkTier{
	{TierID: "in_network", DisplayName: "In Network", IsContracted: true},
	{TierID: "out_network", DisplayName: "Out of Network", IsContracted: false},
}

func applyPlanBenefitSummaryNetworkMatrix(bundle *PatientAPIBundle, el *eligibility.PatientEligibility) bool {
	payload := asStringMap(bundle.Benefit)
	if payload == nil {
		return false
	}

	items := asSlice(payload["benefitSummaryItems"])
	if len(items) == 0 {
		return false
	}
	matrix := buildNetworkMatrix(items)
	if len(matrix) == 0 {
		return false
	}

	el.NetworkTiers = dentaquestNetworkTiers
	el.NetworkMatrix = matrix
	return true
}

type matrixItem struct {
	codeNumber            int
	inNetworkCoinsurance  int
	outNetworkCoinsurance int
}

type matrixClass struct {
	categoryName string
	rangeStart   *int
	rangeEnd     *int
	items        []matrixItem
}

func buildNetworkMatrix(items []any) []eligibility.NetworkMatrixRow {
	byClass := make(map[string]*matrixClass)
	var order []string

	for _, raw := range items {
		item := asStringMap(raw)
		if item == nil {
			continue
		}
		codeNum := parseProcedureCodeNumber(anyStr(item, "procedureCode"))
		if codeNum < 0 {
			continue
		}
		parsed := parseProcedureClass(anyStr(item, "procedureClass"))
		key := parsed.categoryName
		if key == "" {
			key = parsed.raw
		}

		if _, exists := byClass[key]; !exists {
			byClass[key] = &matrixClass{
				categoryName: parsed.categoryName,
				rangeStart:   parsed.rangeStart,
				rangeEnd:     parsed.rangeEnd,
			}
			order = append(order, key)
		} else {
			cls := byClass[key]
			cls.rangeStart = minPtr(cls.rangeStart, parsed.rangeStart)
			cls.rangeEnd = maxPtr(cls.rangeEnd, parsed.rangeEnd)
		}

		byClass[key].items = append(byClass[key].items, matrixItem{
			codeNumber:            codeNum,
			inNetworkCoinsurance:  normalizeCoinsurance(anyStr(item, "inNetworkCoinsurance", "coinsurance")),
			outNetworkCoinsurance: normalizeCoinsurance(anyStr(item, "outNetworkCoinsurance")),
		})
	}

	var rows []eligibility.NetworkMatrixRow
	for _, key := range order {
		cls := byClass[key]
		segments := buildSegments(cls.items)
		inNet := formatSegments(segments, "in_network", cls.rangeStart, cls.rangeEnd)
		outNet := formatSegments(segments, "out_network", cls.rangeStart, cls.rangeEnd)
		if inNet == "" && outNet == "" {
			continue
		}
		rows = append(rows, eligibility.NetworkMatrixRow{
			Name: cls.categoryName,
			Values: map[string]string{
				"in_network":  inNet,
				"out_network": outNet,
			},
		})
	}
	return rows
}

type segment struct {
	start                 int
	end                   int
	inNetworkCoinsurance  int
	outNetworkCoinsurance int
}

func buildSegments(items []matrixItem) []segment {
	if len(items) == 0 {
		return nil
	}
	sorted := make([]matrixItem, len(items))
	copy(sorted, items)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].codeNumber < sorted[j].codeNumber })

	segs := []segment{{
		start:                 sorted[0].codeNumber,
		end:                   sorted[0].codeNumber,
		inNetworkCoinsurance:  sorted[0].inNetworkCoinsurance,
		outNetworkCoinsurance: sorted[0].outNetworkCoinsurance,
	}}
	for _, it := range sorted[1:] {
		prev := &segs[len(segs)-1]
		if it.inNetworkCoinsurance == prev.inNetworkCoinsurance &&
			it.outNetworkCoinsurance == prev.outNetworkCoinsurance {
			prev.end = it.codeNumber
		} else {
			segs = append(segs, segment{
				start:                 it.codeNumber,
				end:                   it.codeNumber,
				inNetworkCoinsurance:  it.inNetworkCoinsurance,
				outNetworkCoinsurance: it.outNetworkCoinsurance,
			})
		}
	}
	return segs
}

func formatSegments(segs []segment, tierID string, rangeStart, rangeEnd *int) string {
	if len(segs) == 0 {
		return ""
	}
	var parts []string
	for i, seg := range segs {
		start := seg.start
		end := seg.end
		if i == 0 && rangeStart != nil {
			start = *rangeStart
		}
		if i == len(segs)-1 && rangeEnd != nil {
			end = *rangeEnd
		} else if i < len(segs)-1 {
			end = max(seg.end, segs[i+1].start-1)
		}
		pct := seg.inNetworkCoinsurance
		if tierID == "out_network" {
			pct = seg.outNetworkCoinsurance
		}
		parts = append(parts, fmt.Sprintf("D%04d-D%04d = %d%%", start, end, pct))
	}
	return strings.Join(parts, " | ")
}

func applyMaximumDeductible(bundle *PatientAPIBundle, el *eligibility.PatientEligibility) {
	for _, raw := range asSlice(bundle.Maximum) {
		record := asStringMap(raw)
		if record == nil {
			continue
		}
		cls := classifyMaxDeductibleRecord(record)
		if cls.specialCase || cls.kind == "" {
			el.OfficeSummary = append(el.OfficeSummary, buildMaxDeductibleOfficeNote(record, cls))
			continue
		}
		el.Accumulators = append(el.Accumulators, buildMaxDeductibleAccumulator(record, cls))
	}
}

type maxDeductibleClassification struct {
	kind        string
	accType     string
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

// parseScopeFromBenefitName extracts "individual" or "family" from the raw
// payer-supplied benefitName string (e.g. "Calendar Family Deductible").
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

func buildTreatmentHistory(payload any, maxRows int) map[string][]eligibility.TreatmentHistoryEntry {
	candidates := collectClinicalHistoryArrays(payload)
	if len(candidates) == 0 {
		return make(map[string][]eligibility.TreatmentHistoryEntry)
	}

	best := candidates[0]
	for _, c := range candidates[1:] {
		if len(c) > len(best) {
			best = c
		}
	}

	rows := mapClinicalHistoryRecords(best, maxRows)
	if len(rows) == 0 {
		return make(map[string][]eligibility.TreatmentHistoryEntry)
	}
	return buildHistoryByCode(rows)
}

func collectClinicalHistoryArrays(v any) [][]any {
	switch typed := v.(type) {
	case []any:
		if looksLikeClinicalHistory(typed) {
			return [][]any{typed}
		}
		var all [][]any
		for _, item := range typed {
			all = append(all, collectClinicalHistoryArrays(item)...)
		}
		return all
	case map[string]any:
		var all [][]any
		for _, val := range typed {
			all = append(all, collectClinicalHistoryArrays(val)...)
		}
		return all
	}
	return nil
}

func looksLikeClinicalHistory(arr []any) bool {
	for _, item := range arr {
		m := asStringMap(item)
		if m == nil {
			continue
		}
		date := anyStr(m, "dateOfService", "serviceDate", "dos", "date")
		code := anyStr(m, "procedureCode", "procCode", "code", "cdtCode")
		if date != "" && code != "" {
			return true
		}
	}
	return false
}

type rawHistoryRow struct {
	ServiceDate   string
	ProcedureCode string
	Description   string
	PartOfMouth   string
}

func mapClinicalHistoryRecords(records []any, max int) []rawHistoryRow {
	var rows []rawHistoryRow
	for _, r := range records {
		m := asStringMap(r)
		if m == nil {
			continue
		}
		row := rawHistoryRow{
			ServiceDate:   normalizeSpace(anyStr(m, "dateOfService", "serviceDate", "dos", "date")),
			ProcedureCode: normalizeSpace(anyStr(m, "procedureCode", "procCode", "code", "cdtCode")),
			Description:   normalizeSpace(anyStr(m, "procedureDescription", "description", "desc")),
			PartOfMouth:   normalizeSpace(anyStr(m, "partOfMouth", "toothArchQuadSurface", "toothSurface", "tooth")),
		}
		if row.ServiceDate == "" || row.ProcedureCode == "" {
			continue
		}
		rows = append(rows, row)
		if len(rows) >= max {
			break
		}
	}
	return rows
}

func buildHistoryByCode(rows []rawHistoryRow) map[string][]eligibility.TreatmentHistoryEntry {
	result := make(map[string][]eligibility.TreatmentHistoryEntry)
	for _, row := range rows {
		parsed := parsePartOfMouth(row.PartOfMouth)
		result[row.ProcedureCode] = append(result[row.ProcedureCode], eligibility.TreatmentHistoryEntry{
			ServiceDate:      row.ServiceDate,
			ToothCode:        parsed.toothCode,
			ToothDescription: parsed.toothDescription,
			Surfaces:         parsed.surfaces,
		})
	}
	return result
}

type partOfMouthParsed struct {
	toothCode        string
	toothDescription string
	surfaces         string
}

func parsePartOfMouth(value string) partOfMouthParsed {
	v := normalizeSpace(value)
	if v == "" || v == "-- / -- / -- / --" {
		return partOfMouthParsed{}
	}
	parts := strings.Split(v, "/")
	get := func(i int) string {
		if i < len(parts) {
			if s := strings.TrimSpace(parts[i]); s != "--" {
				return s
			}
		}
		return ""
	}
	var descParts []string
	if a := get(1); a != "" {
		descParts = append(descParts, a)
	}
	if q := get(2); q != "" {
		descParts = append(descParts, q)
	}
	return partOfMouthParsed{
		toothCode:        get(0),
		toothDescription: strings.Join(descParts, " / "),
		surfaces:         get(3),
	}
}

func normalizeEndDate(value string) string {
	v := normalizeSpace(value)
	if v == "" || v == "9999-12-31" {
		return ""
	}
	return v
}

func setProvision(el *eligibility.PatientEligibility, key, value string) {
	if v := normalizeSpace(value); v != "" {
		if el.Plan.Provisions == nil {
			el.Plan.Provisions = make(map[string]string)
		}
		el.Plan.Provisions[key] = v
	}
}

func firstProbeNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

func interpretEligibilityStatus(value string, current bool) bool {
	v := strings.ToLower(normalizeSpace(value))
	if v == "" {
		return current
	}
	if strings.HasPrefix(v, "active") {
		return true
	}
	if regexp.MustCompile(`inactive|gap|terminated|termination|expired|cancelled|canceled|ineligible`).MatchString(v) {
		return false
	}
	return current
}

func pickCurrentEnrollmentRecord(records []any) map[string]any {
	if len(records) == 0 {
		return nil
	}
	for _, r := range records {
		if m := asStringMap(r); m != nil {
			if strings.ToLower(normalizeSpace(fmt.Sprint(m["status"]))) == "active" {
				return m
			}
		}
	}
	var best map[string]any
	var bestDate string
	for _, r := range records {
		m := asStringMap(r)
		if m == nil {
			continue
		}
		d := normalizeSpace(fmt.Sprint(m["effectiveDate"]))
		if best == nil || d > bestDate {
			best = m
			bestDate = d
		}
	}
	return best
}

var reProcedureCodeNum = regexp.MustCompile(`(?i)^D(\d{4})$`)

func parseProcedureCodeNumber(code string) int {
	m := reProcedureCodeNum.FindStringSubmatch(normalizeSpace(code))
	if len(m) < 2 {
		return -1
	}
	n, _ := strconv.Atoi(m[1])
	return n
}

type parsedProcedureClass struct {
	raw          string
	categoryName string
	rangeStart   *int
	rangeEnd     *int
}

var reProcedureClassRange = regexp.MustCompile(`(?i)^(.*?)\s+D(\d{4})\s*-\s*D(\d{4})$`)

func parseProcedureClass(value string) parsedProcedureClass {
	v := normalizeSpace(value)
	m := reProcedureClassRange.FindStringSubmatch(v)
	if len(m) < 4 {
		return parsedProcedureClass{raw: v, categoryName: normalizeProcedureClassName(v)}
	}
	start, _ := strconv.Atoi(m[2])
	end, _ := strconv.Atoi(m[3])
	return parsedProcedureClass{
		raw:          v,
		categoryName: normalizeProcedureClassName(m[1]),
		rangeStart:   &start,
		rangeEnd:     &end,
	}
}

var procedureClassAliases = map[string]string{
	"diagnostic":                     "Diagnostic",
	"preventive":                     "Preventive",
	"preventive is":                  "Preventive",
	"restorative":                    "Restorative",
	"endodontics":                    "Endodontics",
	"periodontics":                   "Periodontics",
	"prosthodontics removable":       "Prosthodontics; Removable",
	"prosthodontics fixed":           "Prosthodontics; Fixed",
	"implant services":               "Implants",
	"oral surgery":                   "Oral & Maxillofacial Surgery",
	"oral and maxillofacial surgery": "Oral & Maxillofacial Surgery",
	"orthodontics":                   "Orthodontics",
	"adjunctive general services":    "Adjunctive General Services",
}

func normalizeProcedureClassName(value string) string {
	v := normalizeSpace(value)
	lower := strings.ToLower(v)
	if alias, ok := procedureClassAliases[lower]; ok {
		return alias
	}
	return v
}

func normalizeCoinsurance(value string) int {
	n, _ := strconv.Atoi(strings.TrimSpace(value))
	return n
}

func minPtr(a, b *int) *int {
	if a == nil {
		return b
	}
	if b == nil {
		return a
	}
	if *a <= *b {
		return a
	}
	return b
}

func maxPtr(a, b *int) *int {
	if a == nil {
		return b
	}
	if b == nil {
		return a
	}
	if *a >= *b {
		return a
	}
	return b
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
