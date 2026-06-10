package advanced

import (
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"insurance-benefit-agent-go/internal/eligibility"
)

// categoryDefaults provides a fallback coverage percent when a code appears in
// an accumulator's treatment types but not in coverage.categories.  This mirrors
// the old JS CATEGORY_COVERAGE_DEFAULTS table.
var categoryDefaults = map[string]int{
	"preventive":                   100,
	"diagnostic":                   100,
	"restorative":                  50,
	"endodontics":                  50,
	"periodontics":                 50,
	"oral & maxillofacial surgery": 50,
	"prosthodontics; fixed":        50,
	"prosthodontics; removable":    50,
	"implants":                     0,
	"orthodontics":                 50,
	"adjunctive general services":  50,
}

var yearPattern = regexp.MustCompile(`\b(20\d{2})\b`)
var phonePattern = regexp.MustCompile(`\d{3}[-).\s]?\d{3}[-.\s]?\d{4}`)

// Build produces a PatientEligibilityReport from a canonical PatientEligibility
// and two lists of CDT procedure codes:
//   - officeCodes:        codes from the PatCon API for this office
//   - treatmentPlanCodes: codes from the appointment's TreatmentPlanProcCodes field
//
// The two lists are merged and deduplicated internally.
// TP=true is set on codes that appear in treatmentPlanCodes.
func Build(el *eligibility.PatientEligibility, officeCodes []string, treatmentPlanCodes []string) *PatientEligibilityReport {
	if el == nil {
		return nil
	}

	// ── build indexes once ────────────────────────────────────────────────────

	// coverageIdx: upper proc code → CoverageService
	coverageIdx := buildCoverageIndex(el)

	// categoryIdx: upper proc code → category name
	categoryIdx := buildCategoryIndex(el)

	// defaultTierID: TierID matching the patient's default network (e.g. "ppo")
	defaultTierID := resolveDefaultTierID(el)

	// matrixIdx: lower category name → networkRange string for defaultTier
	matrixIdx := buildMatrixIndex(el, defaultTierID)

	// accumIdx: upper proc code → matching Accumulators
	// Includes global accumulators (no treatment types) for every code,
	// plus code-specific matches via AccumulatorTreatmentTypes.ProcedureCodes.

	// ── merge and tag codes ───────────────────────────────────────────────────
	type taggedCode struct {
		code         string
		tp           bool
		inOfficeList bool
	}
	seen := make(map[string]struct{})
	var tagged []taggedCode

	tpSet := make(map[string]struct{}, len(treatmentPlanCodes))
	for _, c := range treatmentPlanCodes {
		k := strings.ToUpper(strings.TrimSpace(c))
		if k != "" {
			tpSet[k] = struct{}{}
		}
	}
	officeSet := make(map[string]struct{}, len(officeCodes))
	for _, c := range officeCodes {
		k := strings.ToUpper(strings.TrimSpace(c))
		if k != "" {
			officeSet[k] = struct{}{}
		}
	}

	addCode := func(raw string, fromTP bool) {
		k := strings.ToUpper(strings.TrimSpace(raw))
		if k == "" {
			return
		}
		if _, exists := seen[k]; exists {
			return
		}
		seen[k] = struct{}{}
		_, isTP := tpSet[k]
		_, isOffice := officeSet[k]
		tagged = append(tagged, taggedCode{code: k, tp: fromTP || isTP, inOfficeList: isOffice})
	}

	for _, c := range officeCodes {
		addCode(c, false)
	}
	for _, c := range treatmentPlanCodes {
		addCode(c, true)
	}

	// Sort for deterministic output.
	sort.Slice(tagged, func(i, j int) bool { return tagged[i].code < tagged[j].code })

	// ── accumulator fallback map: category → coverage default ─────────────────
	// Build a map from proc code to category name using accumulator treatment types
	// (fallback source for codes not in coverage.categories).

	// ── reference date for frequency window computation ────────────────────────
	referenceDate := parseDate(el.Metadata.EligibilityCheckedAt)
	if referenceDate.IsZero() {
		referenceDate = time.Now()
	}
	filteredAccumulators := filterAccumulatorsForReport(el.Accumulators, referenceDate.Year())
	accumIdx, globalAccums := buildAccumulatorIndex(filteredAccumulators)
	accumCodeCategory := buildAccumCodeCategoryIndex(filteredAccumulators)

	// ── per-code enrichment ───────────────────────────────────────────────────
	advCodes := make([]AdvancedCode, 0, len(tagged))
	for _, tc := range tagged {
		allAccums := append(accumIdx[tc.code], globalAccums...) //nolint:gocritic
		advCodes = append(advCodes, enrichCode(
			tc.code, tc.tp, tc.inOfficeList, el, coverageIdx, categoryIdx, accumCodeCategory,
			matrixIdx, allAccums, referenceDate,
		))
	}

	// ── accumulator summaries ─────────────────────────────────────────────────
	var deductibles, maximums []AccumulatorSummary
	for _, acc := range filteredAccumulators {
		sum := AccumulatorSummary{
			ID:        acc.AccumulatorID,
			Name:      acc.Name,
			Note:      acc.Note,
			Kind:      acc.Kind,
			Type:      acc.Type,
			Scope:     acc.Scope,
			Amount:    acc.Amount,
			Used:      acc.Used,
			Remaining: acc.Remaining,
			IsMet:     acc.Remaining <= 0 && acc.Amount > 0,
		}
		switch strings.ToLower(acc.Kind) {
		case "deductible":
			deductibles = append(deductibles, sum)
		case "maximum":
			maximums = append(maximums, sum)
		}
	}

	// ── matrix columns (ordered by network tier) ─────────────────────────────
	matrixCols := make([]MatrixColumn, 0, len(el.NetworkTiers))
	for _, tier := range el.NetworkTiers {
		matrixCols = append(matrixCols, MatrixColumn{
			TierID:      tier.TierID,
			DisplayName: tier.DisplayName,
		})
	}

	// ── matrix rows ───────────────────────────────────────────────────────────
	matrix := make([]MatrixRow, 0, len(el.NetworkMatrix))
	for _, row := range el.NetworkMatrix {
		matrix = append(matrix, MatrixRow{
			Category: row.Name,
			Values:   row.Values,
		})
	}
	// When the payer returns no native category-level coinsurance table (e.g. Aetna via
	// DXC which only provides code-specific coverage rows), synthesize a category matrix
	// from the per-code coverage percentages already in advCodes.
	if len(matrix) == 0 {
		matrix, matrixCols = synthesizeMatrixFromCodes(advCodes, defaultTierID)
	}

	// ── plan provisions (sorted for stable output) ────────────────────────────
	var provisions []Provision
	if len(el.Plan.Provisions) > 0 {
		keys := make([]string, 0, len(el.Plan.Provisions))
		for k := range el.Plan.Provisions {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		provisions = make([]Provision, 0, len(keys))
		for _, k := range keys {
			if value, ok := displayProvision(k, el.Plan.Provisions[k]); ok {
				provisions = append(provisions, Provision{Label: k, Value: value})
			}
		}
	}

	// ── status label ─────────────────────────────────────────────────────────
	statusLabel := strings.TrimSpace(el.Patient.MemberEligibility)
	if statusLabel == "" {
		if el.Patient.IsEligible {
			statusLabel = "Active"
		} else {
			statusLabel = "Not Active"
		}
	}

	return &PatientEligibilityReport{
		Patient: PatientSnapshot{
			FullName:    el.Patient.FullName,
			DateOfBirth: el.Patient.DateOfBirth,
			MemberID:    el.Patient.MemberID,
			GroupNumber: el.Patient.GroupNumber,
			MemberType:  el.Patient.MemberType,
			IsEligible:  el.Patient.IsEligible,
			StatusLabel: statusLabel,
		},
		Plan: PlanSnapshot{
			Carrier:        el.Plan.Carrier,
			PlanName:       el.Plan.PlanName,
			GroupName:      el.Plan.GroupName,
			PlanDesign:     el.Plan.PlanDesign,
			StateRegulated: el.Plan.StateRegulated,
			Provisions:     provisions,
		},
		Network: NetworkSnapshot{
			Type:        el.NetworkInfo.Type,
			DisplayName: el.NetworkInfo.DisplayName,
			DefaultTier: defaultTierID,
		},
		MatrixColumns: matrixCols,
		Matrix:        matrix,
		Deductibles:   deductibles,
		Maximums:      maximums,
		Codes:         advCodes,
		GeneratedAt:   time.Now().UTC().Format("01/02/2006 3:04 PM") + " UTC",
		Source:        el.Metadata.Source,
	}
}

// ── per-code enrichment ───────────────────────────────────────────────────────

func displayProvision(label, value string) (string, bool) {
	label = strings.TrimSpace(label)
	value = strings.Join(strings.Fields(value), " ")
	if label == "" || value == "" {
		return "", false
	}
	lowerLabel := strings.ToLower(label)
	lowerValue := strings.ToLower(value)
	switch lowerLabel {
	case "claimconnect status", "payer option":
		return "", false
	case "customer service":
		if strings.Contains(lowerValue, "new search") || strings.Contains(lowerValue, "view patient") || strings.Contains(lowerValue, "start claim") {
			return "", false
		}
		phone := phonePattern.FindString(value)
		if phone == "" {
			return "", false
		}
		return phone, true
	default:
		return value, true
	}
}

func filterAccumulatorsForReport(accumulators []eligibility.Accumulator, reportYear int) []eligibility.Accumulator {
	if len(accumulators) == 0 {
		return nil
	}
	filtered := make([]eligibility.Accumulator, 0, len(accumulators))
	wantYear := strconv.Itoa(reportYear)
	for _, acc := range accumulators {
		matches := yearPattern.FindAllString(acc.Name, -1)
		if len(matches) == 0 || slices.Contains(matches, wantYear) {
			filtered = append(filtered, acc)
		}
	}
	return filtered
}

func enrichCode(
	code string,
	tp bool,
	inOfficeList bool,
	el *eligibility.PatientEligibility,
	coverageIdx map[string]eligibility.CoverageService,
	categoryIdx map[string]string,
	accumCodeCategory map[string]string,
	matrixIdx map[string]string,
	matchedAccums []eligibility.Accumulator,
	referenceDate time.Time,
) AdvancedCode {
	ac := AdvancedCode{
		Code:         code,
		TP:           tp,
		InOfficeList: inOfficeList,
	}

	// ── resolve category ──────────────────────────────────────────────────────
	category := categoryIdx[code]
	if category == "" {
		// Fallback: category from accumulator treatment type name.
		category = accumCodeCategory[code]
	}
	ac.Category = category
	ac.NetworkRange = matrixIdx[strings.ToLower(category)]

	// ── coverage lookup ───────────────────────────────────────────────────────
	svc, hasCoverage := coverageIdx[code]

	if !hasCoverage {
		// Try fallback coverage percent from category defaults.
		defaultPct := -1
		if category != "" {
			if pct, ok := categoryDefaults[strings.ToLower(category)]; ok {
				defaultPct = pct
			}
		}

		ac.CoveragePercent = defaultPct
		ac.NotCovered = defaultPct == 0
		ac.Accumulators = buildCodeAccumulators(matchedAccums)
		ac.TreatmentHistory = buildHistory(el.TreatmentHistory[code])

		ac.Risk = computeRisk(ac)
		return ac
	}

	ac.Description = svc.Description
	ac.CoveragePercent = svc.CoveragePercent
	ac.PreApprovalRequired = svc.PreAuthorizationRequired
	ac.DeductibleExempted = svc.DeductibleExempted
	ac.MaximumExempted = svc.MaximumExempted
	ac.CopayAmount = svc.CopayAmount
	ac.CrossCheckCodes = svc.CrossCheckCodes

	if svc.CoveragePercent == 0 {
		ac.NotCovered = true
	}

	// Age limits: parse MIN-MAX range and compare against patient's actual age.
	if strings.TrimSpace(svc.AgeLimits) != "" {
		ac.AgeIneligible = isAgeIneligible(svc.AgeLimits, el.Patient.DateOfBirth)
	}

	// Limitations text → []string.
	ac.Limitations = parseLimitationLines(svc.Limitations)

	// Frequency: parse then compute usage from treatment history.
	freq := ParseFrequency(svc.Limitations)
	if freq != nil {
		freq.CounterIdentifier = svc.FrequencyCounterID
		freq.NetworksApplicable = svc.FrequencyNetworks
		computeFrequencyUsage(code, svc.Limitations, freq, el.TreatmentHistory, referenceDate)
	}
	ac.Frequency = freq

	// Accumulators matched by CDT code.
	ac.Accumulators = buildCodeAccumulators(matchedAccums)

	// Risk.
	ac.Risk = computeRisk(ac)

	// Treatment history.
	ac.TreatmentHistory = buildHistory(el.TreatmentHistory[code])

	return ac
}

// ── frequency usage computation ───────────────────────────────────────────────

// computeFrequencyUsage fills the computed fields on freq by cross-referencing
// treatment history.  It resolves family codes (e.g. all exam types share a
// single frequency counter) and counts entries within the active period window.
func computeFrequencyUsage(
	procCode string,
	limitation string,
	freq *CodeFrequency,
	history map[string][]eligibility.TreatmentHistoryEntry,
	referenceDate time.Time,
) {
	// Resolve which codes' history to count (may be a family).
	familyCodes := resolveFrequencyFamilyCodes(procCode, limitation)

	// Collect all history entries for the family, tagged with their source code.
	var entries []dated
	for _, fc := range familyCodes {
		for _, e := range history[fc] {
			d := parseDate(e.ServiceDate)
			if d.IsZero() {
				continue
			}
			if inFrequencyWindow(d, referenceDate, freq) {
				entries = append(entries, dated{date: d, sourceCode: fc})
			}
		}
	}

	// Deduplicate by (sourceCode + date) in case scraper noise creates duplicates.
	deduped := deduplicateEntries(entries)

	// Sort descending so [0] is most recent.
	sort.Slice(deduped, func(i, j int) bool {
		return deduped[i].date.After(deduped[j].date)
	})

	allowed := 1
	if freq.Allowed != nil {
		allowed = *freq.Allowed
	}

	freq.Used = len(deduped)
	freq.Remaining = maxInt(allowed-freq.Used, 0)
	freq.Exceeded = freq.Used >= allowed

	if len(deduped) > 0 {
		last := deduped[0]
		freq.LastServiceDate = last.date.Format("2006-01-02")
		if freq.PeriodType == "calendar" {
			freq.UsageSummary = fmt.Sprintf("%d of %d used in %d; %d remaining (last: %s)",
				freq.Used, allowed, referenceDate.Year(), freq.Remaining, freq.LastServiceDate)
		} else if freq.PeriodType == "lifetime" {
			freq.UsageSummary = fmt.Sprintf("%d lifetime use(s) found; last: %s",
				freq.Used, freq.LastServiceDate)
		} else {
			freq.UsageSummary = fmt.Sprintf("%d of %d used; %d remaining (last: %s)",
				freq.Used, allowed, freq.Remaining, freq.LastServiceDate)
		}
	}

	if freq.Exceeded {
		freq.Message = fmt.Sprintf("Frequency met: %d of %d already used (last: %s).",
			freq.Used, allowed, freq.LastServiceDate)
	}
}

func resolveFrequencyFamilyCodes(procCode, limitation string) []string {
	for _, rule := range FrequencyFamilyRules {
		if rule.Pattern.MatchString(limitation) {
			return rule.Codes
		}
	}
	return []string{procCode}
}

func inFrequencyWindow(serviceDate, referenceDate time.Time, freq *CodeFrequency) bool {
	switch freq.PeriodType {
	case "lifetime":
		return !serviceDate.After(referenceDate)
	case "calendar":
		return serviceDate.Year() == referenceDate.Year()
	default: // rolling
		windowStart := referenceDate.AddDate(0, -freq.PeriodMonths, 0)
		return !serviceDate.Before(windowStart) && !serviceDate.After(referenceDate)
	}
}

type dated struct {
	date       time.Time
	sourceCode string
}

func deduplicateEntries(entries []dated) []dated {
	seen := make(map[string]struct{}, len(entries))
	var out []dated
	for _, e := range entries {
		key := e.sourceCode + "|" + e.date.Format("2006-01-02")
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, e)
	}
	return out
}

// ── risk computation ──────────────────────────────────────────────────────────

// computeRisk applies the agreed rules:
//   - UNKNOWN: no procedure-level coverage evidence was returned
//   - DENIED:  explicit notCovered, ageIneligible, coveragePercent==0, or frequency exceeded
//   - RISKY:   any matched accumulator maximum is fully met (IsMet)
//   - ACTIVE:  everything else (covered, within limits, no met maximums)
func computeRisk(ac AdvancedCode) CodeRisk {
	if ac.CoveragePercent < 0 {
		return CodeRisk{
			Level:  "UNKNOWN",
			Reason: "Coverage not returned",
			Detail: "The captured eligibility response did not include procedure-level coverage for this code",
			Color:  "#6b7280",
		}
	}

	// DENIED — age restriction.
	if ac.AgeIneligible {
		return CodeRisk{
			Level:  "DENIED",
			Reason: "Age restriction",
			Detail: "Coverage is not available for this patient's age",
			Color:  "#e74c3c",
		}
	}

	// DENIED — not covered or zero coverage percent.
	if ac.NotCovered || ac.CoveragePercent == 0 {
		return CodeRisk{
			Level:  "DENIED",
			Reason: "Not covered by plan",
			Detail: "This procedure is not covered by the patient's insurance plan",
			Color:  "#e74c3c",
		}
	}

	// DENIED — frequency limit exceeded.
	if ac.Frequency != nil && ac.Frequency.Exceeded {
		return CodeRisk{
			Level:  "DENIED",
			Reason: "Frequency exceeded",
			Detail: ac.Frequency.Message,
			Color:  "#e74c3c",
		}
	}

	// RISKY — any matched accumulator maximum has been fully met.
	for _, ca := range ac.Accumulators {
		if strings.ToLower(ca.Kind) == "maximum" && ca.IsMet {
			return CodeRisk{
				Level:  "RISKY",
				Reason: "Confirm remaining benefit",
				Detail: fmt.Sprintf("Annual or lifetime maximum may be reached (accumulator: %s)", ca.ID),
				Color:  "#f59e0b",
			}
		}
	}

	// ACTIVE — covered, in limits, no financial concerns.
	return CodeRisk{
		Level:  "ACTIVE",
		Reason: "Active",
		Color:  "#27ae60",
	}
}

// ── index builders ────────────────────────────────────────────────────────────

func buildCoverageIndex(el *eligibility.PatientEligibility) map[string]eligibility.CoverageService {
	idx := make(map[string]eligibility.CoverageService)
	for _, cat := range el.Coverage.Categories {
		for _, svc := range cat.Services {
			key := strings.ToUpper(strings.TrimSpace(svc.Code))
			if key != "" {
				idx[key] = svc
			}
		}
	}
	return idx
}

func buildCategoryIndex(el *eligibility.PatientEligibility) map[string]string {
	idx := make(map[string]string)
	for _, cat := range el.Coverage.Categories {
		for _, svc := range cat.Services {
			key := strings.ToUpper(strings.TrimSpace(svc.Code))
			if key != "" {
				idx[key] = cat.Name
			}
		}
	}
	return idx
}

// buildAccumCodeCategoryIndex maps proc code (upper) → treatment type name
// from accumulator treatment types.  Used as a fallback when the code is not
// in coverage.categories.
func buildAccumCodeCategoryIndex(accumulators []eligibility.Accumulator) map[string]string {
	idx := make(map[string]string)
	for _, acc := range accumulators {
		for _, tt := range acc.AccumulatorTreatmentTypes {
			for _, pc := range tt.ProcedureCodes {
				key := strings.ToUpper(strings.TrimSpace(pc))
				if key != "" && idx[key] == "" {
					idx[key] = tt.Name
				}
			}
		}
	}
	return idx
}

// resolveDefaultTierID returns the TierID matching the patient's default network.
func resolveDefaultTierID(el *eligibility.PatientEligibility) string {
	target := strings.ToLower(strings.TrimSpace(el.NetworkInfo.Type))
	targetStripped := strings.TrimPrefix(target, "##")

	for _, tier := range el.NetworkTiers {
		if strings.ToLower(tier.TierID) == targetStripped {
			return tier.TierID
		}
	}
	for _, tier := range el.NetworkTiers {
		if strings.Contains(strings.ToLower(tier.DisplayName), targetStripped) {
			return tier.TierID
		}
	}
	for _, tier := range el.NetworkTiers {
		if tier.IsContracted {
			return tier.TierID
		}
	}
	if len(el.NetworkTiers) > 0 {
		return el.NetworkTiers[0].TierID
	}
	return ""
}

func buildMatrixIndex(el *eligibility.PatientEligibility, defaultTierID string) map[string]string {
	idx := make(map[string]string, len(el.NetworkMatrix))
	for _, row := range el.NetworkMatrix {
		if v, ok := row.Values[defaultTierID]; ok {
			idx[strings.ToLower(row.Name)] = v
		}
	}
	return idx
}

// synthesizeMatrixFromCodes builds a category-level matrix when the payer provides
// no native coinsurance table. It groups advCodes by standard CDT category ranges,
// computes the modal coverage percentage per category (excluding not-covered codes),
// and returns a single in-network column.
func synthesizeMatrixFromCodes(codes []AdvancedCode, tierID string) ([]MatrixRow, []MatrixColumn) {
	type bucket struct {
		order   int
		pctFreq map[int]int
	}
	buckets := map[string]*bucket{}

	for _, c := range codes {
		if c.CoveragePercent < 0 || c.NotCovered {
			continue
		}
		cat, order := cdtCategoryAndOrder(c.Code)
		if cat == "" {
			continue
		}
		b, ok := buckets[cat]
		if !ok {
			b = &bucket{order: order, pctFreq: map[int]int{}}
			buckets[cat] = b
		}
		b.pctFreq[c.CoveragePercent]++
	}

	if len(buckets) == 0 {
		return nil, nil
	}

	type catRow struct {
		order int
		cat   string
		pct   int
	}
	var rows []catRow
	for cat, b := range buckets {
		mode, maxFreq := -1, 0
		for pct, freq := range b.pctFreq {
			if freq > maxFreq || (freq == maxFreq && pct > mode) {
				mode = pct
				maxFreq = freq
			}
		}
		if mode < 0 {
			continue
		}
		rows = append(rows, catRow{order: b.order, cat: cat, pct: mode})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].order < rows[j].order })

	matrixRows := make([]MatrixRow, 0, len(rows))
	for _, r := range rows {
		matrixRows = append(matrixRows, MatrixRow{
			Category: r.cat,
			Values:   map[string]string{tierID: fmt.Sprintf("%d%%", r.pct)},
		})
	}

	displayName := "In-Network"
	if strings.ToLower(tierID) != "in" {
		displayName = strings.ToUpper(tierID[:1]) + strings.ToLower(tierID[1:])
	}
	return matrixRows, []MatrixColumn{{TierID: tierID, DisplayName: displayName}}
}

func cdtCategoryAndOrder(code string) (string, int) {
	code = strings.ToUpper(strings.TrimSpace(code))
	if len(code) < 2 || code[0] != 'D' {
		return "", 0
	}
	n, err := strconv.Atoi(code[1:])
	if err != nil {
		return "", 0
	}
	switch {
	case n < 1000:
		return "Diagnostic", 1
	case n < 2000:
		return "Preventive", 2
	case n < 2700:
		return "Basic Restorative", 3
	case n < 3000:
		return "Major Restorative", 4
	case n < 4000:
		return "Endodontics", 5
	case n < 5000:
		return "Periodontics", 6
	case n < 6000:
		return "Prosthodontics (Removable)", 7
	case n < 6200:
		return "Implant Services", 8
	case n < 7000:
		return "Prosthodontics (Fixed)", 9
	case n < 8000:
		return "Oral & Maxillofacial Surgery", 10
	case n < 9000:
		return "Orthodontics", 11
	default:
		return "Adjunctive Services", 12
	}
}

// buildAccumulatorIndex returns:
//   - a map of upper proc code → accumulators that list that code in ProcedureCodes
//   - a slice of "global" accumulators (no AccumulatorTreatmentTypes) that apply to every code
func buildAccumulatorIndex(accumulators []eligibility.Accumulator) (map[string][]eligibility.Accumulator, []eligibility.Accumulator) {
	idx := make(map[string][]eligibility.Accumulator)
	var global []eligibility.Accumulator

	for _, acc := range accumulators {
		if len(acc.AccumulatorTreatmentTypes) == 0 {
			// No treatment types → applies globally to all codes.
			global = append(global, acc)
			continue
		}
		for _, tt := range acc.AccumulatorTreatmentTypes {
			if len(tt.ProcedureCodes) == 0 {
				// Treatment type has no CDT codes listed — skip (not global).
				continue
			}
			for _, pc := range tt.ProcedureCodes {
				key := strings.ToUpper(strings.TrimSpace(pc))
				if key != "" {
					idx[key] = append(idx[key], acc)
				}
			}
		}
	}
	return idx, global
}

// ── helpers ───────────────────────────────────────────────────────────────────

func parseLimitationLines(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	var lines []string
	for _, part := range strings.FieldsFunc(raw, func(r rune) bool { return r == ';' || r == '\n' }) {
		if t := strings.TrimSpace(part); t != "" {
			lines = append(lines, t)
		}
	}
	sort.Strings(lines)
	return lines
}

// isAgeIneligible parses an age limit string like "11 - 999" or "0 - 12" and
// returns true only if the patient's computed age falls outside that range.
// Returns false if either the limit or DOB cannot be parsed (no false positives).
func isAgeIneligible(ageLimits, dateOfBirth string) bool {
	parts := strings.SplitN(ageLimits, "-", 2)
	if len(parts) != 2 {
		return false
	}
	minAge, err1 := strconv.Atoi(strings.TrimSpace(parts[0]))
	maxAge, err2 := strconv.Atoi(strings.TrimSpace(parts[1]))
	if err1 != nil || err2 != nil {
		return false
	}
	dob, err := time.Parse("01/02/2006", dateOfBirth)
	if err != nil {
		dob, err = time.Parse("2006-01-02", dateOfBirth)
		if err != nil {
			return false
		}
	}
	now := time.Now()
	age := now.Year() - dob.Year()
	if now.YearDay() < dob.YearDay() {
		age--
	}
	return age < minAge || age > maxAge
}

func buildCodeAccumulators(accums []eligibility.Accumulator) []CodeAccumulator {
	if len(accums) == 0 {
		return nil
	}
	// Deduplicate by AccumulatorID (global accumulators may be appended multiple times).
	seen := make(map[string]struct{}, len(accums))
	out := make([]CodeAccumulator, 0, len(accums))
	for _, acc := range accums {
		if _, exists := seen[acc.AccumulatorID]; exists {
			continue
		}
		seen[acc.AccumulatorID] = struct{}{}
		out = append(out, CodeAccumulator{
			ID:        acc.AccumulatorID,
			Kind:      acc.Kind,
			Type:      acc.Type,
			Amount:    acc.Amount,
			Remaining: acc.Remaining,
			Applies:   true,
			IsMet:     acc.Remaining <= 0 && acc.Amount > 0,
		})
	}
	return out
}

func buildHistory(entries []eligibility.TreatmentHistoryEntry) []HistoryEntry {
	if len(entries) == 0 {
		return nil
	}
	out := make([]HistoryEntry, 0, len(entries))
	for _, e := range entries {
		out = append(out, HistoryEntry{
			ServiceDate:      e.ServiceDate,
			ToothCode:        e.ToothCode,
			ToothDescription: e.ToothDescription,
			Surfaces:         e.Surfaces,
		})
	}
	return out
}

// deduplicateCodes is exported for callers that want to pre-merge code lists.
func deduplicateCodes(codes []string) []string {
	seen := make(map[string]struct{}, len(codes))
	out := make([]string, 0, len(codes))
	for _, c := range codes {
		key := strings.ToUpper(strings.TrimSpace(c))
		if key == "" {
			continue
		}
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, key)
	}
	slices.Sort(out)
	return out
}

func parseDate(value string) time.Time {
	v := strings.TrimSpace(value)
	if v == "" {
		return time.Time{}
	}
	layouts := []string{"2006-01-02", "01/02/2006", "1/2/2006", "2006/01/02", time.RFC3339}
	for _, layout := range layouts {
		if t, err := time.Parse(layout, v); err == nil {
			return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
		}
	}
	return time.Time{}
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
