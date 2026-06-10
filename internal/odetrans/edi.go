// Package odetrans builds Open Dental-compatible synthetic eligibility X12.
package odetrans

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode"

	"insurance-benefit-agent-go/internal/advanced"
	"insurance-benefit-agent-go/internal/models"
)

const (
	serviceDentalCare  = "35"
	serviceOrtho       = "38"
	serviceHealthPlan  = "30"
	serviceDiagnostic  = "23"
	serviceXRay        = "4"
	servicePreventive  = "41"
	serviceRestorative = "25"
	serviceEndo        = "26"
	servicePerio       = "24"
	serviceOralSurgery = "40"
	serviceCrowns      = "36"
	serviceProsth      = "39"
	serviceAdjunctive  = "28"
	serviceMaxProsth   = "27"
	serviceAccident    = "37"
	serviceAnesthesia  = "7"
	serviceDiagLab     = "5"
	benefitActive      = "1"
	benefitInactive    = "6"
	benefitDeductible  = "C"
	benefitLimitation  = "F"
	periodCalendarYear = "23"
	periodRemaining    = "29"
)

// BuildInput contains the fields needed to synthesize OD-readable 270/271 text.
type BuildInput struct {
	Appointment models.Appointment
	Report      *advanced.PatientEligibilityReport
	Status      string
	PayerURL    string
	Credential  models.CredentialCandidate
	Provider    ProviderIdentity
	Practice    PracticeIdentity
	Now         time.Time
}

// Pair is the generated request/response X12 pair.
type Pair struct {
	Request270  string
	Response271 string
}

// WritePairFiles writes synthetic 270 and 271 EDI files for active/inactive results.
func WritePairFiles(outputDir string, input BuildInput) (Pair, bool, error) {
	if !ShouldGenerate(input.Status, input.Report) {
		return Pair{}, false, nil
	}
	pair := BuildPair(input)
	if pair.Request270 == "" || pair.Response271 == "" {
		return Pair{}, false, nil
	}
	if err := os.MkdirAll(outputDir, 0o755); err != nil {
		return Pair{}, false, fmt.Errorf("create output dir: %w", err)
	}
	base := artifactBase(input.Appointment)
	if err := os.WriteFile(filepath.Join(outputDir, base+"_270Request.edi"), []byte(pair.Request270), 0o644); err != nil {
		return Pair{}, false, fmt.Errorf("write 270 edi: %w", err)
	}
	if err := os.WriteFile(filepath.Join(outputDir, base+"_271Response.edi"), []byte(pair.Response271), 0o644); err != nil {
		return Pair{}, false, fmt.Errorf("write 271 edi: %w", err)
	}
	insertSQL := BuildInsertSQL(input.Appointment, pair)
	if err := os.WriteFile(filepath.Join(outputDir, base+"_etrans_insert.sql"), []byte(insertSQL), 0o644); err != nil {
		return Pair{}, false, fmt.Errorf("write etrans insert sql: %w", err)
	}
	return pair, true, nil
}

// ShouldGenerate limits synthetic OD EDI to definitive eligibility outcomes.
func ShouldGenerate(status string, report *advanced.PatientEligibilityReport) bool {
	if report == nil {
		return false
	}
	status = strings.ToLower(strings.TrimSpace(status))
	if status == "verified" || status == "inactive" {
		return true
	}
	label := strings.ToLower(strings.TrimSpace(report.Patient.StatusLabel))
	return strings.Contains(label, "active") || strings.Contains(label, "inactive")
}

// BuildPair creates a synthetic 270 request and matching 271 response.
func BuildPair(input BuildInput) Pair {
	if input.Report == nil {
		return Pair{}
	}
	now := input.Now
	if now.IsZero() {
		now = time.Now()
	}
	ctx := buildContext(input, now)
	return Pair{
		Request270:  build270(ctx),
		Response271: build271(ctx, input.Report, input.Status),
	}
}

type ediContext struct {
	now       time.Time
	date      string
	shortDate string
	time      string
	control   string
	trace     string
	payerName string
	payerID   string
	provLast  string
	provFirst string
	provID    string
	provTIN   string
	address   string
	city      string
	state     string
	zip       string
	taxonomy  string
	patLast   string
	patFirst  string
	memberID  string
	groupNum  string
	dob       string
}

func buildContext(input BuildInput, now time.Time) ediContext {
	appt := input.Appointment
	report := input.Report
	payerName := firstNonEmpty(report.Plan.Carrier, appt.CarrierName, input.PayerURL, "UNKNOWN PAYER")
	provider := normalizeProviderIdentity(input.Provider)
	practice := normalizePracticeIdentity(input.Practice)
	provFirst := provider.FirstName
	provLast := provider.LastName
	if provFirst == "" || provLast == "" {
		provFirst, provLast = splitProviderName(firstNonEmpty(input.Credential.ProviderName, "PROVIDER"))
	}
	provTIN := firstNonEmpty(digitsOnly(provider.TaxID), digitsOnly(input.Credential.ProviderTIN), "000000000")
	provNPI := firstNonEmpty(digitsOnly(provider.NPI), provTIN, "000000000")
	controlSeed := digitsOnly(appt.PatNum + appt.AptNum + firstNonEmpty(appt.Ordinal, "1"))
	if controlSeed == "" {
		controlSeed = strconv.FormatInt(now.Unix()%1000000000, 10)
	}
	control := leftPad(controlSeed, 9)
	return ediContext{
		now:       now,
		date:      now.Format("20060102"),
		shortDate: now.Format("060102"),
		time:      now.Format("1504"),
		control:   control,
		trace:     firstNonEmpty(controlSeed, "1"),
		payerName: ediValue(payerName),
		payerID:   firstNonEmpty(appt.PayerID, "00000"),
		provFirst: ediValue(provFirst),
		provLast:  ediValue(provLast),
		provID:    provNPI,
		provTIN:   provTIN,
		address:   ediValue(practice.Address),
		city:      ediValue(practice.City),
		state:     ediValue(practice.State),
		zip:       digitsOnly(practice.Zip),
		taxonomy:  firstNonEmpty(ediValue(provider.Taxonomy), defaultDentalTaxonomy),
		patLast:   ediValue(firstNonEmpty(appt.LName, lastName(report.Patient.FullName))),
		patFirst:  ediValue(firstNonEmpty(appt.FName, firstName(report.Patient.FullName))),
		memberID:  ediValue(firstNonEmpty(report.Patient.MemberID, appt.SubscriberID, appt.SSN)),
		groupNum:  ediValue(firstNonEmpty(report.Patient.GroupNumber, appt.GroupNum)),
		dob:       normalizeEDIDate(firstNonEmpty(report.Patient.DateOfBirth, appt.DOB)),
	}
}

func build270(ctx ediContext) string {
	segs := []string{
		isa("ZZ", ctx.provTIN, "30", "330989922", ctx.shortDate, ctx.time, ctx.control),
		fmt.Sprintf("GS*HS*%s*330989922*%s*%s*%s*X*004010X092", ctx.provTIN, ctx.date, ctx.time, strings.TrimLeft(ctx.control, "0")),
		"ST*270*0001",
		fmt.Sprintf("BHT*0022*13*%s*%s*%s", ctx.trace, ctx.date, ctx.time),
		"HL*1**20*1",
		fmt.Sprintf("NM1*PR*2*%s*****PI*%s", ctx.payerName, ctx.payerID),
		"HL*2*1*21*1",
		fmt.Sprintf("NM1*1P*1*%s*%s****XX*%s", ctx.provLast, ctx.provFirst, ctx.provID),
		fmt.Sprintf("REF*TJ*%s", ctx.provTIN),
	}
	if ctx.address != "" {
		segs = append(segs, "N3*"+ctx.address)
	}
	if ctx.city != "" || ctx.state != "" || ctx.zip != "" {
		segs = append(segs, fmt.Sprintf("N4*%s*%s*%s", ctx.city, ctx.state, ctx.zip))
	}
	if ctx.taxonomy != "" {
		segs = append(segs, "PRV*PE*ZZ*"+ctx.taxonomy)
	}
	segs = append(segs,
		"HL*3*2*22*0",
		fmt.Sprintf("TRN*1*%s*1%s", ctx.trace, ctx.provTIN),
		fmt.Sprintf("NM1*IL*1*%s*%s****MI*%s", ctx.patLast, ctx.patFirst, ctx.memberID),
	)
	if ctx.groupNum != "" {
		segs = append(segs, "REF*6P*"+ctx.groupNum)
	}
	if ctx.dob != "" {
		segs = append(segs, "DMG*D8*"+ctx.dob)
	}
	segs = append(segs,
		"DTP*307*D8*"+ctx.date,
		"EQ*35",
	)
	segs = append(segs, fmt.Sprintf("SE*%d*0001", countTransactionSegments(segs)+1))
	segs = append(segs,
		fmt.Sprintf("GE*1*%s", strings.TrimLeft(ctx.control, "0")),
		"IEA*1*"+ctx.control,
	)
	return strings.Join(segs, "~") + "~"
}

func build271(ctx ediContext, report *advanced.PatientEligibilityReport, status string) string {
	segs := []string{
		isa("30", "330989922", "ZZ", ctx.provTIN, ctx.shortDate, ctx.time, ctx.control),
		fmt.Sprintf("GS*HB*330989922*%s*%s*%s*%s*X*004010X092A1", ctx.provTIN, ctx.date, ctx.time, strings.TrimLeft(ctx.control, "0")),
		"ST*271*0001",
		fmt.Sprintf("BHT*0022*11*%sWEB*%s*%s", ctx.trace, ctx.date, ctx.time),
		"HL*1**20*1",
		fmt.Sprintf("NM1*PR*2*%s*****PI*%s", ctx.payerName, ctx.payerID),
		"HL*2*1*21*1",
		fmt.Sprintf("NM1*1P*1*%s*%s****XX*%s", ctx.provLast, ctx.provFirst, ctx.provID),
		"HL*3*2*22*0",
		fmt.Sprintf("TRN*2*%s*1%s", ctx.trace, ctx.provTIN),
		fmt.Sprintf("NM1*IL*1*%s*%s****MI*%s", ctx.patLast, ctx.patFirst, ctx.memberID),
	}
	if ctx.groupNum != "" {
		segs = append(segs, "REF*6P*"+ctx.groupNum)
	}
	if ctx.dob != "" {
		segs = append(segs, "DMG*D8*"+ctx.dob)
	}
	if isInactive(status, report) {
		segs = append(segs, "EB*"+benefitInactive+"**"+serviceHealthPlan, "MSG*Inactive")
	} else {
		segs = append(segs, "EB*"+benefitActive+"**"+serviceHealthPlan)
		segs = append(segs, coinsuranceSegments(report)...)
		segs = append(segs, accumulatorSegments(report)...)
	}
	segs = append(segs, fmt.Sprintf("SE*%d*0001", countTransactionSegments(segs)+1))
	segs = append(segs,
		fmt.Sprintf("GE*1*%s", strings.TrimLeft(ctx.control, "0")),
		"IEA*1*"+ctx.control,
	)
	return strings.Join(segs, "~") + "~"
}

func coinsuranceSegments(report *advanced.PatientEligibilityReport) []string {
	if report == nil {
		return nil
	}
	tierID := strings.TrimSpace(report.Network.DefaultTier)
	if tierID == "" && len(report.MatrixColumns) > 0 {
		tierID = strings.TrimSpace(report.MatrixColumns[0].TierID)
	}
	var segs []string
	seen := map[string]bool{}
	for _, row := range report.Matrix {
		services := serviceCodesFromCategory(row.Category)
		if len(services) == 0 {
			continue
		}
		coverage := matrixCoverageValue(row.Values, tierID)
		coveragePct, ok := parseCoveragePercent(coverage)
		if !ok || coveragePct < 0 || coveragePct > 100 {
			continue
		}
		patientPct := 100 - coveragePct
		for _, service := range services {
			if service == "" || seen[service] {
				continue
			}
			segs = append(segs,
				fmt.Sprintf("EB*A*IND*%s***%s**%s", service, periodCalendarYear, ediPercentValue(patientPct)),
				"MSG*"+ediValue(row.Category+" Coinsurance"),
			)
			seen[service] = true
		}
	}
	return segs
}

func matrixCoverageValue(values map[string]string, tierID string) string {
	if len(values) == 0 {
		return ""
	}
	if tierID != "" {
		if value := strings.TrimSpace(values[tierID]); value != "" {
			return value
		}
	}
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func parseCoveragePercent(value string) (int, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, false
	}
	for i := 0; i < len(value); i++ {
		if value[i] < '0' || value[i] > '9' {
			continue
		}
		start := i
		for i < len(value) && value[i] >= '0' && value[i] <= '9' {
			i++
		}
		n, err := strconv.Atoi(value[start:i])
		if err != nil {
			return 0, false
		}
		return n, true
	}
	return 0, false
}

func accumulatorSegments(report *advanced.PatientEligibilityReport) []string {
	var segs []string
	seen := map[string]bool{}
	for _, acc := range report.Maximums {
		if isEmptyMaximum(acc) {
			continue
		}
		for _, service := range accumulatorServiceCodes(acc) {
			if service == "" || acc.Remaining < 0 {
				continue
			}
			network := accumulatorNetworkIndicator(acc)
			remainingEB := fmt.Sprintf("EB*%s*%s*%s***%s*%s", benefitLimitation, scopeCode(acc.Scope), service, periodRemaining, moneyValue(acc.Remaining))
			appendAccumulatorSegment(&segs, seen,
				remainingEB,
				ebWithNetworkIndicator(remainingEB, network, 7),
				"MSG*"+ediValue(acc.Name),
			)
			if acc.Amount > 0 {
				totalEB := fmt.Sprintf("EB*%s*%s*%s***%s*%s", benefitLimitation, scopeCode(acc.Scope), service, accumulatorAmountPeriod(acc), moneyValue(acc.Amount))
				appendAccumulatorSegment(&segs, seen,
					totalEB,
					ebWithNetworkIndicator(totalEB, network, 7),
					"MSG*"+ediValue(acc.Name+" Total"),
				)
			}
		}
	}
	for _, acc := range report.Deductibles {
		for _, service := range accumulatorServiceCodes(acc) {
			if service == "" {
				continue
			}
			network := accumulatorNetworkIndicator(acc)
			if acc.Amount > 0 {
				amountEB := fmt.Sprintf("EB*%s*%s*%s***%s*%s", benefitDeductible, scopeCode(acc.Scope), service, periodCalendarYear, moneyValue(acc.Amount))
				appendAccumulatorSegment(&segs, seen,
					amountEB,
					ebWithNetworkIndicator(amountEB, network, 7),
					"MSG*"+ediValue(acc.Name),
				)
			}
			if acc.Remaining >= 0 {
				remainingEB := fmt.Sprintf("EB*%s*%s*%s***%s*%s", benefitDeductible, scopeCode(acc.Scope), service, periodRemaining, moneyValue(acc.Remaining))
				appendAccumulatorSegment(&segs, seen,
					remainingEB,
					ebWithNetworkIndicator(remainingEB, network, 7),
					"MSG*"+ediValue(acc.Name+" Remaining"),
				)
			}
		}
	}
	return segs
}

func appendAccumulatorSegment(segs *[]string, seen map[string]bool, dedupeKey, eb, msg string) {
	if seen[dedupeKey] {
		return
	}
	*segs = append(*segs, eb, msg)
	seen[dedupeKey] = true
}

func accumulatorNetworkIndicator(acc advanced.AccumulatorSummary) string {
	name := strings.ToLower(acc.Name)
	switch {
	case strings.Contains(name, "oon") || strings.Contains(name, "out-of-network"):
		return "N"
	case strings.Contains(name, "in-network"):
		return "Y"
	case strings.TrimSpace(acc.Name) != "":
		return "Y"
	default:
		return ""
	}
}

func ebWithNetworkIndicator(eb, indicator string, populatedThrough int) string {
	indicator = strings.TrimSpace(strings.ToUpper(indicator))
	if indicator != "Y" && indicator != "N" {
		return eb
	}
	fields := strings.Split(eb, "*")
	for len(fields) <= 12 {
		fields = append(fields, "")
	}
	fields[12] = indicator
	return strings.Join(trimTrailingEmptyFields(fields, maxInt(populatedThrough, 12)), "*")
}

func trimTrailingEmptyFields(fields []string, minFields int) []string {
	for len(fields) > minFields+1 && fields[len(fields)-1] == "" {
		fields = fields[:len(fields)-1]
	}
	return fields
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func isEmptyMaximum(acc advanced.AccumulatorSummary) bool {
	return acc.Amount == 0 && acc.Remaining == 0
}

func accumulatorAmountPeriod(acc advanced.AccumulatorSummary) string {
	if strings.EqualFold(strings.TrimSpace(acc.Type), "lifetime") {
		return "32"
	}
	return periodCalendarYear
}

func accumulatorServiceCodes(acc advanced.AccumulatorSummary) []string {
	text := acc.Name + " " + strings.Join(acc.Services, " ")
	if codes := serviceCodesFromCategory(text); len(codes) > 0 {
		if shared := sharedServiceCodes(acc.Note); len(shared) > 0 && shouldExpandSharedMaximum(acc) {
			return uniqueStrings(append(codes, shared...))
		}
		return codes
	}
	if shared := sharedServiceCodes(acc.Note); len(shared) > 0 && shouldExpandSharedMaximum(acc) {
		return shared
	}
	return []string{serviceDentalCare}
}

func shouldExpandSharedMaximum(acc advanced.AccumulatorSummary) bool {
	return strings.EqualFold(strings.TrimSpace(acc.Kind), "maximum") && strings.Contains(strings.ToLower(acc.Note), "shared with")
}

func sharedServiceCodes(note string) []string {
	text := strings.ToLower(note)
	if !strings.Contains(text, "shared with") {
		return nil
	}
	sharedText := text
	if idx := strings.Index(sharedText, "shared with"); idx >= 0 {
		sharedText = sharedText[idx+len("shared with"):]
	}
	parts := strings.FieldsFunc(sharedText, func(r rune) bool {
		return r == ',' || r == ';' || r == ':' || r == '/' || r == '&'
	})
	var codes []string
	for _, part := range parts {
		for _, piece := range strings.Split(part, " and ") {
			codes = append(codes, serviceCodesFromCategory(piece)...)
		}
	}
	return uniqueStrings(codes)
}

func serviceCodesFromCategory(category string) []string {
	text := strings.ToLower(category)
	switch {
	case strings.Contains(text, "oral exam") || strings.Contains(text, "exam") && strings.Contains(text, "x-ray"):
		return []string{serviceDiagnostic, serviceXRay}
	case strings.Contains(text, "ortho"):
		return []string{serviceOrtho}
	case strings.Contains(text, "lab"):
		return []string{serviceDiagLab}
	case strings.Contains(text, "diagnostic") && (strings.Contains(text, "x-ray") || strings.Contains(text, "xray") || strings.Contains(text, "x ray")):
		return []string{serviceXRay}
	case strings.Contains(text, "diagnostic"):
		return []string{serviceDiagnostic}
	case strings.Contains(text, "x-ray") || strings.Contains(text, "xray") || strings.Contains(text, "x ray") || strings.Contains(text, "pano") || strings.Contains(text, "fmx"):
		return []string{serviceXRay}
	case strings.Contains(text, "routine") || strings.Contains(text, "prevent") || strings.Contains(text, "sealant") || strings.Contains(text, "fluoride") || strings.Contains(text, "prophy"):
		return []string{servicePreventive}
	case strings.Contains(text, "major restorative"):
		return []string{serviceCrowns}
	case strings.Contains(text, "restorative") || strings.Contains(text, "filling"):
		return []string{serviceRestorative}
	case strings.Contains(text, "endodont") || strings.Contains(text, "root canal"):
		return []string{serviceEndo}
	case strings.Contains(text, "periodont") || strings.Contains(text, "perio") || strings.Contains(text, "gum treatment") || strings.Contains(text, "gum") || strings.Contains(text, "srp"):
		return []string{servicePerio}
	case strings.Contains(text, "oral surgery") || strings.Contains(text, "maxillofacial surgery"):
		return []string{serviceOralSurgery}
	case strings.Contains(text, "extraction"):
		return []string{serviceOralSurgery}
	case strings.Contains(text, "maxillofacial prosth"):
		return []string{serviceMaxProsth}
	case strings.Contains(text, "accident"):
		return []string{serviceAccident}
	case strings.Contains(text, "anesthesia"):
		return []string{serviceAnesthesia}
	case strings.Contains(text, "implant"):
		return []string{serviceProsth}
	case strings.Contains(text, "crown") && strings.Contains(text, "bridge"):
		return []string{serviceCrowns, serviceProsth}
	case strings.Contains(text, "crown"):
		return []string{serviceCrowns}
	case strings.Contains(text, "inlay") || strings.Contains(text, "onlay"):
		if strings.Contains(text, "bridge") {
			return []string{serviceCrowns, serviceProsth}
		}
		return []string{serviceCrowns}
	case strings.Contains(text, "bridge") || strings.Contains(text, "denture") || strings.Contains(text, "prosthodont"):
		return []string{serviceProsth}
	case strings.Contains(text, "miscellaneous") || strings.Contains(text, "adjunctive") || strings.Contains(text, "other"):
		return []string{serviceAdjunctive}
	case strings.Contains(text, "dental care") || strings.Contains(text, "general"):
		return []string{serviceDentalCare}
	case strings.Contains(text, "dental"):
		return []string{serviceDentalCare}
	default:
		return nil
	}
}

func uniqueStrings(values []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(values))
	for _, value := range values {
		if value == "" || seen[value] {
			continue
		}
		out = append(out, value)
		seen[value] = true
	}
	return out
}

func isInactive(status string, report *advanced.PatientEligibilityReport) bool {
	if strings.EqualFold(strings.TrimSpace(status), "Inactive") {
		return true
	}
	label := strings.ToLower(strings.TrimSpace(report.Patient.StatusLabel))
	return strings.Contains(label, "inactive") || strings.Contains(label, "not active")
}

func scopeCode(scope string) string {
	switch strings.ToLower(strings.TrimSpace(scope)) {
	case "family":
		return "FAM"
	default:
		return "IND"
	}
}

func isa(senderQualifier, sender, receiverQualifier, receiver, date, tm, control string) string {
	return strings.Join([]string{
		"ISA",
		"00",
		padRight("", 10),
		"00",
		padRight("", 10),
		senderQualifier,
		padRight(sender, 15),
		receiverQualifier,
		padRight(receiver, 15),
		date,
		tm,
		"U",
		"00401",
		control,
		"0",
		"P",
		":",
	}, "*")
}

func countTransactionSegments(segs []string) int {
	count := 0
	for _, seg := range segs {
		if strings.HasPrefix(seg, "ST*") || count > 0 {
			count++
		}
	}
	return count
}

func artifactBase(appt models.Appointment) string {
	aptSegment := sanitizeSegment(appt.AptNum)
	if strings.TrimSpace(appt.AptNum) == "" {
		parts := []string{"noappt", "ord" + sanitizeSegment(firstNonEmpty(appt.Ordinal, "1"))}
		if strings.TrimSpace(appt.InsSubNum) != "" {
			parts = append(parts, "sub"+sanitizeSegment(appt.InsSubNum))
		}
		if strings.TrimSpace(appt.PlanNum) != "" {
			parts = append(parts, "plan"+sanitizeSegment(appt.PlanNum))
		}
		aptSegment = strings.Join(parts, "_")
	}
	return strings.Join([]string{
		sanitizeSegment(appt.PatNum),
		aptSegment,
		"ord" + sanitizeSegment(firstNonEmpty(appt.Ordinal, "1")),
	}, "_")
}

func sanitizeSegment(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "unknown"
	}
	var b strings.Builder
	for _, r := range value {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '-' || r == '_' || r == '.' {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	return strings.Trim(b.String(), "._-")
}

func moneyValue(value float64) string {
	if math.IsNaN(value) || math.IsInf(value, 0) || value < 0 {
		return "0.00"
	}
	return strconv.FormatFloat(value, 'f', 2, 64)
}

func ediPercentValue(percent int) string {
	if percent <= 0 {
		return "0"
	}
	if percent >= 100 {
		return "1"
	}
	return strconv.FormatFloat(float64(percent)/100, 'f', -1, 64)
}

func splitProviderName(value string) (first, last string) {
	parts := strings.Fields(strings.ReplaceAll(value, ",", " "))
	if len(parts) == 0 {
		return "PROVIDER", "PROVIDER"
	}
	if len(parts) == 1 {
		return parts[0], parts[0]
	}
	return parts[0], parts[len(parts)-1]
}

func firstName(value string) string {
	parts := strings.Fields(value)
	if len(parts) == 0 {
		return ""
	}
	return parts[0]
}

func lastName(value string) string {
	parts := strings.Fields(value)
	if len(parts) == 0 {
		return ""
	}
	return parts[len(parts)-1]
}

func normalizeEDIDate(value string) string {
	value = strings.TrimSpace(value)
	for _, layout := range []string{"2006-01-02", "01-02-2006", "01/02/2006", "20060102"} {
		if t, err := time.Parse(layout, value); err == nil {
			return t.Format("20060102")
		}
	}
	return digitsOnly(value)
}

func ediValue(value string) string {
	value = strings.ToUpper(strings.Join(strings.Fields(strings.TrimSpace(value)), " "))
	replacer := strings.NewReplacer("*", " ", "~", " ", ":", " ", "^", " ")
	return strings.TrimSpace(replacer.Replace(value))
}

func digitsOnly(value string) string {
	var b strings.Builder
	for _, r := range value {
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func leftPad(value string, width int) string {
	if len(value) >= width {
		return value[len(value)-width:]
	}
	return strings.Repeat("0", width-len(value)) + value
}

func padRight(value string, width int) string {
	if len(value) >= width {
		return value[:width]
	}
	return value + strings.Repeat(" ", width-len(value))
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}
