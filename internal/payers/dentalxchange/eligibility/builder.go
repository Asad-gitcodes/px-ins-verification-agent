package eligibility

import (
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"insurance-benefit-agent-go/internal/eligibility"
	dxapi "insurance-benefit-agent-go/internal/payers/dentalxchange/api"

	"golang.org/x/net/html"
)

func BuildEligibilityFromProbe(bundle *dxapi.ProbeBundle) *eligibility.PatientEligibility {
	if bundle == nil {
		return nil
	}
	appt := bundle.Appointment
	fullName := strings.TrimSpace(appt.FName + " " + appt.LName)
	memberType := "Subscriber"
	if strings.TrimSpace(appt.Relationship) != "" && strings.TrimSpace(appt.Relationship) != "0" {
		memberType = "Dependent"
	}

	eligPairs := extractTablePairs(bundle.EligibilityPage.HTML)
	benPairs := extractTablePairs(bundle.BenefitsPage.HTML)

	el := &eligibility.PatientEligibility{
		Patient: eligibility.PatientInfo{
			FullName:   dentalXChangePatientName(bundle, benPairs, eligPairs, fullName),
			MemberType: memberType,
			DateOfBirth: normalizeDate(firstNonEmpty(
				tablePairGet(benPairs, "date of birth"),
				tablePairGet(eligPairs, "date of birth"),
				labelValue(bundle.BenefitsPage.Text, "Date of Birth"),
				labelValue(bundle.EligibilityPage.Text, "Date of Birth"),
				appt.DOB,
			)),
			MemberID: firstNonEmpty(
				tablePairGet(benPairs, "member id or ssn", "member id"),
				tablePairGet(eligPairs, "member id or ssn", "member id"),
				labelValue(bundle.BenefitsPage.Text, "Member ID or SSN"),
				labelValue(bundle.EligibilityPage.Text, "Member ID or SSN"),
				appt.SubscriberID,
			),
			GroupNumber: firstNonEmpty(
				tablePairGet(benPairs, "group#", "group number", "group num"),
				tablePairGet(eligPairs, "group#", "group number", "group num"),
				labelValue(bundle.BenefitsPage.Text, "Group#"),
				labelValue(bundle.EligibilityPage.Text, "Group Number"),
				appt.GroupNum,
			),
			IsEligible: resolveEligible(bundle),
		},
		Plan: eligibility.PlanInfo{
			Carrier: firstNonEmpty(
				payerLabelName(bundle.SearchRequest.PayerLabel),
				cleanDXValue(tablePairGet(benPairs, "payer name", "payer")),
				cleanDXValue(tablePairGet(eligPairs, "payer name", "payer")),
				appt.CarrierName,
			),
			PlanName: extractDXPlanName(benPairs, bundle.BenefitsPage.Text),
			GroupName: firstNonEmpty(
				tablePairGet(benPairs, "group name"),
				tablePairGet(eligPairs, "group name"),
				labelValue(bundle.BenefitsPage.Text, "Group Name"),
				appt.GroupName,
			),
			Provisions: make(map[string]string),
		},
		Coverage:      eligibility.Coverage{Categories: []eligibility.CoverageCategory{}},
		NetworkTiers:  defaultNetworkTiers(),
		NetworkMatrix: []eligibility.NetworkMatrixRow{},
		Accumulators:  []eligibility.Accumulator{},
		OfficeSummary: []eligibility.OfficeSummaryNote{},
		Metadata: eligibility.Metadata{
			EligibilityCheckedAt: time.Now().UTC().Format(time.RFC3339),
			Source:               "DentalXChangeClaimConnect",
		},
	}
	el.Patient.MemberEligibility = activeLabel(bundle)
	el.NetworkInfo.Type = "in"
	el.NetworkInfo.DisplayName = "In-Network"
	el.NetworkInfo.Confidence = 1
	el.NetworkInfo.Reason = "ClaimConnect benefits page parsed with default in-network tier"

	setProvision(el, "ClaimConnect status", bundle.EligibilityPage.Status)
	setProvision(el, "Payer option", bundle.SearchRequest.PayerLabel)
	if v := firstNonEmpty(tablePairGet(eligPairs, "subscriber name", "subscriber"), labelValue(bundle.EligibilityPage.Text, "Subscriber Name")); v != "" {
		setProvision(el, "Subscriber", v)
	}
	enrichFromBenefitsHTML(el, bundle.BenefitsPage.HTML)
	el.Accumulators = dedupeAccumulators(el.Accumulators)
	el.NetworkMatrix = dedupeMatrixRows(el.NetworkMatrix)
	return el
}

func resolveEligible(bundle *dxapi.ProbeBundle) bool {
	if bundle == nil {
		return false
	}
	text := strings.ToLower(bundle.EligibilityPage.Text + " " + bundle.BenefitsPage.Text)
	if hasDXInactiveSignal(text) {
		return false
	}
	// Primary: CSS class indicators in HTML are definitive
	if bundle.EligibilityPage.HTML != "" {
		cls := extractStatusClass(bundle.EligibilityPage.HTML)
		lower := strings.ToLower(cls)
		if strings.Contains(lower, "inactive") || strings.Contains(lower, "terminated") {
			return false
		}
		if strings.Contains(lower, "active") {
			return true
		}
	}
	// Secondary: text-based detection
	status := strings.ToLower(activeLabel(bundle))
	if hasDXInactiveSignal(status) {
		return false
	}
	return strings.Contains(text, "status: active") || strings.Contains(text, "coverage:") && !strings.Contains(text, "inactive")
}

func activeLabel(bundle *dxapi.ProbeBundle) string {
	text := strings.ToLower(bundle.EligibilityPage.Text + " " + bundle.BenefitsPage.Text)
	if hasDXInactiveSignal(text) {
		return "Inactive"
	}
	// HTML-based: look for greenb/redb class text (most reliable for ClaimConnect)
	if bundle.EligibilityPage.HTML != "" {
		if status := extractStatusClass(bundle.EligibilityPage.HTML); status != "" {
			return status
		}
	}
	return firstNonEmpty(labelValue(bundle.EligibilityPage.Text, "Status"), labelValue(bundle.BenefitsPage.Text, "Coverage"))
}

func hasDXInactiveSignal(text string) bool {
	text = strings.ToLower(text)
	for _, marker := range []string{
		"patient terminated",
		"subscriber terminated",
		"member terminated",
		"coverage terminated",
		"status: inactive",
		"not active",
	} {
		if strings.Contains(text, marker) {
			return true
		}
	}
	return false
}

// extractStatusClass finds the text content of the first element with class "greenb" or "redb".
// ClaimConnect renders the eligibility status ("Active", "Inactive") in a <td class="greenb"> cell.
func extractStatusClass(pageHTML string) string {
	if strings.TrimSpace(pageHTML) == "" {
		return ""
	}
	doc, err := html.Parse(strings.NewReader(pageHTML))
	if err != nil {
		return ""
	}
	var result string
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if result != "" {
			return
		}
		if n.Type == html.ElementNode {
			for _, a := range n.Attr {
				if strings.EqualFold(a.Key, "class") {
					cls := strings.ToLower(a.Val)
					if strings.Contains(cls, "greenb") || strings.Contains(cls, "redb") {
						if text := strings.TrimSpace(nodeText(n)); text != "" {
							result = text
						}
					}
				}
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)
	return result
}

// extractTablePairs walks all <tr> elements and maps adjacent label→value cell pairs.
// Handles both 2-column tables (label | value) and 4-column tables (label | value | label | value).
func extractTablePairs(pageHTML string) map[string]string {
	if strings.TrimSpace(pageHTML) == "" {
		return nil
	}
	doc, err := html.Parse(strings.NewReader(pageHTML))
	if err != nil {
		return nil
	}
	out := map[string]string{}
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode && n.Data == "tr" {
			var tdNodes []*html.Node
			for c := n.FirstChild; c != nil; c = c.NextSibling {
				if c.Type == html.ElementNode && c.Data == "td" {
					tdNodes = append(tdNodes, c)
				}
			}
			// ClaimConnect pattern: 2-column row where one cell holds <br>-separated
			// labels and the other holds <br>-separated values in parallel.
			if len(tdNodes) == 2 && (cellHasBR(tdNodes[0]) || cellHasBR(tdNodes[1])) {
				labels := brSplitCell(tdNodes[0])
				values := brSplitCell(tdNodes[1])
				for i := 0; i < len(labels) && i < len(values); i++ {
					label := strings.TrimRight(strings.TrimSpace(labels[i]), ":")
					value := strings.TrimSpace(values[i])
					if label != "" && value != "" && len(label) < 60 {
						out[strings.ToLower(label)] = value
					}
				}
			} else {
				// Standard path: extract adjacent label→value pairs (2-col and 4-col rows)
				var cells []string
				for _, td := range tdNodes {
					cells = append(cells, strings.TrimRight(strings.TrimSpace(nodeText(td)), ":"))
				}
				for i := 0; i+1 < len(cells); i += 2 {
					label := cells[i]
					value := cells[i+1]
					if label != "" && value != "" && len(label) < 60 {
						out[strings.ToLower(label)] = value
					}
				}
			}
			// Always recurse into children to reach nested inner tables.
			for c := n.FirstChild; c != nil; c = c.NextSibling {
				walk(c)
			}
			return
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)
	return out
}

// cellHasBR returns true if the node contains a <br> element anywhere in its subtree.
func cellHasBR(n *html.Node) bool {
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if c.Type == html.ElementNode && strings.ToLower(c.Data) == "br" {
			return true
		}
		if cellHasBR(c) {
			return true
		}
	}
	return false
}

// brSplitCell returns the text fragments from a cell split by <br> elements,
// preserving empty slots so parallel label/value columns stay aligned.
func brSplitCell(n *html.Node) []string {
	var parts []string
	var current strings.Builder
	hasSplit := false
	var walk func(*html.Node)
	walk = func(node *html.Node) {
		if node.Type == html.ElementNode {
			switch strings.ToLower(node.Data) {
			case "br":
				hasSplit = true
				parts = append(parts, strings.TrimSpace(current.String()))
				current.Reset()
				return
			case "script", "style", "noscript":
				return
			}
		}
		if node.Type == html.TextNode {
			current.WriteString(node.Data)
		}
		for c := node.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(n)
	if hasSplit {
		parts = append(parts, strings.TrimSpace(current.String()))
	} else if s := strings.TrimSpace(current.String()); s != "" {
		parts = []string{s}
	}
	return parts
}

// tablePairGet looks up keys in a table-pairs map (case-insensitive).
// Tries exact match first, then substring match for keys ≥ 4 chars.
func tablePairGet(pairs map[string]string, keys ...string) string {
	if pairs == nil {
		return ""
	}
	for _, key := range keys {
		k := strings.ToLower(strings.TrimSpace(key))
		if v, ok := pairs[k]; ok && strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	// Substring fallback: stored key contains search key
	for _, key := range keys {
		k := strings.ToLower(strings.TrimSpace(key))
		if len(k) < 4 {
			continue
		}
		for pk, pv := range pairs {
			if strings.Contains(pk, k) && strings.TrimSpace(pv) != "" {
				return strings.TrimSpace(pv)
			}
		}
	}
	return ""
}

func dentalXChangePatientName(bundle *dxapi.ProbeBundle, benPairs, eligPairs map[string]string, appointmentName string) string {
	if bundle == nil {
		return cleanPersonName(appointmentName)
	}
	return firstNonEmpty(
		patientNameFromPairs(benPairs),
		patientNameFromPairs(eligPairs),
		cleanPersonName(labelValue(bundle.BenefitsPage.Text, "Patient Name")),
		cleanPersonName(labelValue(bundle.BenefitsPage.Text, "Member Name")),
		cleanPersonName(labelValue(bundle.BenefitsPage.Text, "Dependent Name")),
		cleanPersonName(labelValue(bundle.EligibilityPage.Text, "Patient Name")),
		cleanPersonName(labelValue(bundle.EligibilityPage.Text, "Member Name")),
		cleanPersonName(labelValue(bundle.EligibilityPage.Text, "Dependent Name")),
		cleanPersonName(bundle.SearchRequest.PatientName),
		cleanPersonName(appointmentName),
		cleanPersonName(strings.TrimSpace(bundle.Appointment.SubFName+" "+bundle.Appointment.SubLName)),
	)
}

func patientNameFromPairs(pairs map[string]string) string {
	if pairs == nil {
		return ""
	}
	if v := patientPairGet(pairs, "patient name", "member name", "dependent name", "covered person name"); v != "" {
		return v
	}
	if v := joinNameParts(
		patientPairGet(pairs, "patient first name", "member first name", "dependent first name", "first name", "given name"),
		patientPairGet(pairs, "patient last name", "member last name", "dependent last name", "last name", "surname"),
	); v != "" {
		return v
	}
	return ""
}

func patientPairGet(pairs map[string]string, keys ...string) string {
	for _, key := range keys {
		k := strings.ToLower(strings.TrimSpace(key))
		if v, ok := pairs[k]; ok {
			if cleaned := cleanPersonName(v); cleaned != "" {
				return cleaned
			}
		}
	}
	for _, key := range keys {
		k := strings.ToLower(strings.TrimSpace(key))
		if len(k) < 4 {
			continue
		}
		for pk, pv := range pairs {
			if !strings.Contains(pk, k) || excludedPatientNameKey(pk) {
				continue
			}
			if cleaned := cleanPersonName(pv); cleaned != "" {
				return cleaned
			}
		}
	}
	return ""
}

func joinNameParts(first, last string) string {
	return cleanPersonName(strings.TrimSpace(strings.Join([]string{first, last}, " ")))
}

func excludedPatientNameKey(key string) bool {
	key = strings.ToLower(strings.TrimSpace(key))
	for _, marker := range []string{"payer", "provider", "group", "plan", "customer", "employer", "subscriber"} {
		if strings.Contains(key, marker) {
			return true
		}
	}
	return false
}

func cleanPersonName(value string) string {
	value = strings.Join(strings.Fields(strings.TrimSpace(value)), " ")
	value = strings.Trim(value, ":,")
	if value == "" {
		return ""
	}
	lower := strings.ToLower(value)
	if strings.Contains(lower, "<![cdata") || strings.Contains(value, "{") || strings.Contains(value, "}") {
		return ""
	}
	for _, marker := range []string{"member id", "group", "payer", "provider", "customer service", "date of birth", "benefit", "eligibility", "status", "coverage"} {
		if strings.Contains(lower, marker) {
			return ""
		}
	}
	for _, placeholder := range []string{"member", "subscriber", "patient", "dependent", "insured"} {
		if lower == placeholder {
			return ""
		}
	}
	if regexp.MustCompile(`^\d+$`).MatchString(value) || regexp.MustCompile(`^\d{1,2}[/-]\d{1,2}[/-]\d{2,4}$`).MatchString(value) {
		return ""
	}
	return value
}

func enrichFromBenefitsHTML(el *eligibility.PatientEligibility, pageHTML string) {
	if el == nil || strings.TrimSpace(pageHTML) == "" {
		return
	}
	doc, err := html.Parse(strings.NewReader(pageHTML))
	if err != nil {
		return
	}
	sections := findBenefitSections(doc)
	if categories := buildTemplateCoverageCategories(sections); len(categories) > 0 {
		el.Coverage.Categories = categories
	}
	for _, section := range sections {
		rows := section.Rows
		if len(rows) == 0 {
			continue
		}
		switch classifyBenefitSection(section) {
		case "coinsurance":
			el.NetworkMatrix = append(el.NetworkMatrix, buildMatrix(rows)...)
			if len(el.Coverage.Categories) == 0 {
				el.Coverage.Categories = buildCoverageCategories(rows)
			}
		case "deductible":
			el.Accumulators = append(el.Accumulators, buildDeductibles(rows)...)
		case "maximum":
			el.Accumulators = append(el.Accumulators, buildMaximums(rows)...)
		}
	}
	addLimitationProvisions(el, sections)
}

type dxBenefitSection struct {
	Title string
	Rows  [][]string
}

func classifyBenefitSection(section dxBenefitSection) string {
	headers, _ := findHeader(section.Rows)
	if len(headers) == 0 {
		return ""
	}
	idx := headerIndex(headers)
	title := strings.ToLower(section.Title)
	all := strings.ToLower(flattenRows(section.Rows))
	hasService := firstIndex(idx, "service type", "service", "procedure code", "type") >= 0
	hasPercent := firstIndex(idx, "percentage", "percent") >= 0
	hasTierPercent := len(coinsuranceTierPercentColumns(idx)) > 0
	hasAmount := firstIndex(idx, "contract amount", "calendar year amount", "lifetime amount", "amount", "remaining amount", "remaining") >= 0

	switch {
	case strings.Contains(title, "deductible"):
		return "deductible"
	case strings.Contains(title, "maximum") || strings.Contains(title, "limitation"):
		return "maximum"
	case hasAmount && strings.Contains(all, "deductible"):
		return "deductible"
	case hasAmount && (strings.Contains(all, "maximum") || strings.Contains(all, "lifetime") ||
		strings.Contains(all, "service year") || strings.Contains(all, "year to date") || strings.Contains(all, "remaining")):
		return "maximum"
	case hasService && hasPercent:
		return "coinsurance"
	case strings.Contains(title, "co-insurance") || strings.Contains(title, "coinsurance"):
		return "coinsurance"
	case hasService && hasTierPercent:
		return "coinsurance"
	default:
		return ""
	}
}

func buildTemplateCoverageCategories(sections []dxBenefitSection) []eligibility.CoverageCategory {
	if len(sections) == 0 {
		return nil
	}
	collector := newCoverageCollector()
	principalPct := map[string]int{}

	for _, section := range sections {
		if classifyBenefitSection(section) != "coinsurance" {
			continue
		}
		headers, start := findHeader(section.Rows)
		idx := headerIndex(headers)
		serviceIdx := firstIndex(idx, "service type", "service")
		participationIdx := firstIndex(idx, "participation")
		networkIdx := firstIndex(idx, "network")
		percentIdx := firstIndex(idx, "percentage", "percent")
		messageIdx := firstIndex(idx, "message")
		if serviceIdx < 0 || percentIdx < 0 {
			continue
		}

		// Principal-style: category percentage rows with "Participation" carrying network status.
		if messageIdx < 0 && networkIdx < 0 && participationIdx >= 0 {
			for _, row := range section.Rows[start:] {
				if networkID(cell(row, participationIdx)) != "in" {
					continue
				}
				service := cell(row, serviceIdx)
				if service == "" || isCDTCode(service) {
					continue
				}
				if pct := parseSingleInsurancePct(cell(row, percentIdx)); pct >= 0 {
					principalPct[strings.ToLower(service)] = pct
				}
			}
		}
	}

	for _, section := range sections {
		title := strings.ToLower(section.Title)
		headers, start := findHeader(section.Rows)
		if len(headers) == 0 {
			continue
		}
		idx := headerIndex(headers)

		switch {
		case strings.Contains(title, "service level benefits") && sectionAppliesInNetwork(title):
			addAetnaServiceLevelCoverage(collector, section.Rows, idx, start)
		case classifyBenefitSection(section) == "coinsurance":
			addCoinsuranceCoverage(collector, section.Rows, idx, start)
		case strings.Contains(title, "limitations and maximums"):
			addPrincipalLimitationsCoverage(collector, section.Rows, idx, start, principalPct)
		case strings.Contains(title, "non-covered") || strings.Contains(title, "non covered"):
			addNonCoveredCoverage(collector, section.Rows, idx, start)
		}
	}

	return collector.categories()
}

func addAetnaServiceLevelCoverage(c *coverageCollector, rows [][]string, idx map[string]int, start int) {
	codeIdx := firstIndex(idx, "procedure code", "service type", "service")
	percentIdx := firstIndex(idx, "percentage", "percent")
	limitIdx := firstIndex(idx, "frequency", "limitation")
	messageIdx := firstIndex(idx, "message")
	if codeIdx < 0 {
		return
	}
	for _, row := range rows[start:] {
		codeText := cell(row, codeIdx)
		codes := cdtCodeRE.FindAllString(codeText, -1)
		if len(codes) == 0 {
			continue
		}
		message := cell(row, messageIdx)
		limits := joinNonEmpty(cell(row, limitIdx), message)
		pct := parseInsurancePctFlexible(cell(row, percentIdx), false)
		if pct < 0 && strings.Contains(strings.ToLower(message), "not covered") {
			pct = 0
		}
		if pct < 0 {
			continue
		}
		for _, code := range codes {
			c.add(strings.TrimSpace(codeText), eligibility.CoverageService{
				Code:               strings.ToUpper(code),
				CoveragePercent:    pct,
				Limitations:        limits,
				DeductibleExempted: strings.Contains(strings.ToLower(message), "deductible does not apply"),
			}, true)
		}
	}
}

func addCoinsuranceCoverage(c *coverageCollector, rows [][]string, idx map[string]int, start int) {
	serviceIdx := firstIndex(idx, "service type", "service")
	participationIdx := firstIndex(idx, "participation")
	networkIdx := firstIndex(idx, "network")
	percentIdx := firstIndex(idx, "percentage", "percent")
	messageIdx := firstIndex(idx, "message")
	if serviceIdx < 0 {
		return
	}
	if percentIdx < 0 {
		addTierColumnCoinsuranceCoverage(c, rows, serviceIdx, messageIdx, start)
		return
	}
	isDeltaMichigan := participationIdx >= 0 && networkIdx >= 0 && messageIdx < 0
	for _, row := range rows[start:] {
		service := cell(row, serviceIdx)
		if service == "" {
			continue
		}
		networkText := firstNonEmpty(cell(row, participationIdx), cell(row, networkIdx))
		if networkID(networkText) != "in" {
			continue
		}
		percent := cell(row, percentIdx)
		message := cell(row, messageIdx)
		pct := parseInsurancePctFlexible(percent, isDeltaMichigan)
		if pct < 0 {
			continue
		}

		codes := cdtCodeRE.FindAllString(service+" "+message, -1)
		if len(codes) == 0 {
			continue
		}
		category := service
		if isCDTCode(service) {
			category = "Procedure Code"
		}
		for _, code := range codes {
			c.add(category, eligibility.CoverageService{
				Code:            strings.ToUpper(code),
				CoveragePercent: pct,
			}, isCDTCode(service))
		}
	}
}

func addTierColumnCoinsuranceCoverage(c *coverageCollector, rows [][]string, serviceIdx, messageIdx, start int) {
	headers, _ := findHeader(rows)
	tierCols := coinsuranceTierPercentColumns(headerIndex(headers))
	inIdx, ok := tierCols["in"]
	if !ok {
		return
	}
	for _, row := range rows[start:] {
		service := cell(row, serviceIdx)
		message := cell(row, messageIdx)
		if service == "" {
			continue
		}
		pct := parseInsurancePctFlexible(cell(row, inIdx), true)
		if pct < 0 {
			continue
		}
		codes := cdtCodeRE.FindAllString(service+" "+message, -1)
		if len(codes) == 0 {
			continue
		}
		category := service
		if isCDTCode(service) {
			category = "Procedure Code"
		}
		for _, code := range codes {
			c.add(category, eligibility.CoverageService{
				Code:            strings.ToUpper(code),
				CoveragePercent: pct,
			}, isCDTCode(service))
		}
	}
}

func addPrincipalLimitationsCoverage(c *coverageCollector, rows [][]string, idx map[string]int, start int, categoryPct map[string]int) {
	serviceIdx := firstIndex(idx, "service type", "service")
	codeIdx := firstIndex(idx, "procedure code")
	deliveryIdx := firstIndex(idx, "delivery pattern")
	participationIdx := firstIndex(idx, "participation")
	messageIdx := firstIndex(idx, "message")
	if serviceIdx < 0 {
		return
	}
	if codeIdx < 0 {
		codeIdx = serviceIdx
	}
	for _, row := range rows[start:] {
		codeText := cell(row, codeIdx)
		codes := cdtCodeRE.FindAllString(codeText, -1)
		if len(codes) == 0 {
			continue
		}
		service := cell(row, serviceIdx)
		participation := strings.ToLower(cell(row, participationIdx))
		pct := -1
		if strings.Contains(participation, "not eligible") {
			pct = 0
		} else if v, ok := categoryPct[strings.ToLower(service)]; ok {
			pct = v
		} else if mapped, ok := categoryPct[strings.ToLower(dxCategoryAlias(service))]; ok {
			pct = mapped
		}
		limits := joinNonEmpty(cell(row, deliveryIdx), cell(row, messageIdx))
		for _, code := range codes {
			c.add(firstNonEmpty(service, "Procedure Code"), eligibility.CoverageService{
				Code:            strings.ToUpper(code),
				CoveragePercent: pct,
				Limitations:     limits,
			}, false)
		}
	}
}

func addNonCoveredCoverage(c *coverageCollector, rows [][]string, idx map[string]int, start int) {
	serviceIdx := firstIndex(idx, "procedure code", "service type", "service")
	messageIdx := firstIndex(idx, "message")
	if serviceIdx < 0 {
		return
	}
	for _, row := range rows[start:] {
		text := joinNonEmpty(cell(row, serviceIdx), cell(row, messageIdx))
		for _, code := range cdtCodeRE.FindAllString(text, -1) {
			c.add("Non-Covered", eligibility.CoverageService{
				Code:            strings.ToUpper(code),
				CoveragePercent: 0,
				Limitations:     cell(row, messageIdx),
			}, false)
		}
	}
}

func addLimitationProvisions(el *eligibility.PatientEligibility, sections []dxBenefitSection) {
	if el == nil {
		return
	}
	for _, section := range sections {
		if !strings.Contains(strings.ToLower(section.Title), "limitation") && classifyBenefitSection(section) != "maximum" {
			continue
		}
		headers, start := findHeader(section.Rows)
		if len(headers) == 0 {
			continue
		}
		idx := headerIndex(headers)
		serviceIdx := firstIndex(idx, "service type", "service")
		messageIdx := firstIndex(idx, "message")
		deliveryIdx := firstIndex(idx, "delivery pattern", "delivery", "frequency")
		inNetworkIdx := exactHeaderIndex(idx, "in-network", "in network")
		if serviceIdx < 0 {
			continue
		}
		for _, row := range section.Rows[start:] {
			service := cell(row, serviceIdx)
			message := cell(row, messageIdx)
			delivery := cell(row, deliveryIdx)
			inNetwork := cell(row, inNetworkIdx)
			if service == "" || isCDTCode(service) || hasMoney(row) {
				continue
			}
			value := joinNonEmpty(message, delivery, inNetwork)
			if value == "" || strings.EqualFold(value, service) {
				continue
			}
			setProvision(el, "Limitation: "+service, value)
		}
	}
}

func buildDeductibles(rows [][]string) []eligibility.Accumulator {
	return buildAmountAccumulators(rows, "deductible")
}

func buildMaximums(rows [][]string) []eligibility.Accumulator {
	return buildAmountAccumulators(rows, "maximum")
}

func buildAmountAccumulators(rows [][]string, kind string) []eligibility.Accumulator {
	headers, start := findHeader(rows)
	if len(headers) == 0 {
		return nil
	}
	idx := headerIndex(headers)
	serviceIdx := firstIndex(idx, "service type", "service", "type", "procedure code")
	networkIdx := networkTierIndex(idx)
	coverageIdx := firstIndex(idx, "coverage", "level")
	amountCols := accumulatorAmountColumns(idx, kind)
	periodIdx := firstIndex(idx, "period")
	messageIdx := firstIndex(idx, "message", "note", "detail")
	deliveryIdx := firstIndex(idx, "delivery pattern", "delivery", "frequency")
	if len(amountCols) == 0 {
		return nil
	}

	var out []eligibility.Accumulator
	for _, row := range rows[start:] {
		for _, amountCol := range amountCols {
			if len(row) <= amountCol.amountIdx {
				continue
			}
			amount := parseMoney(row[amountCol.amountIdx])
			if amount < 0 && strings.EqualFold(kind, "maximum") && strings.Contains(strings.ToLower(cell(row, periodIdx)), "remaining") {
				amount = parseMoney(cell(row, amountCol.remainingIdx))
			}
			if amount < 0 {
				continue
			}
			remaining := amount
			if amountCol.remainingIdx >= 0 && len(row) > amountCol.remainingIdx {
				if rem := parseMoney(row[amountCol.remainingIdx]); rem >= 0 {
					remaining = rem
				}
			}
			service := cell(row, serviceIdx)
			scope := scopeFromText(cell(row, coverageIdx))
			// Handle "Individual - Service Name" or "Family - Service Name" in a single cell.
			if coverageIdx < 0 || coverageIdx == serviceIdx {
				if scope, service = splitCoverageService(service); scope == "" {
					scope = "individual"
				}
			}
			network := firstNonEmpty(cell(row, networkIdx), amountCol.network)
			note := parseAccumulatorNote(cell(row, messageIdx), cell(row, deliveryIdx))
			out = append(out, eligibility.Accumulator{
				AccumulatorID: strings.Join([]string{"dentalxchange", kind, amountCol.accType, scope, networkID(network), slug(service)}, "_"),
				Name:          accumulatorName(kind, amountCol.accType, scope, network, service),
				Note:          note,
				Kind:          kind,
				Type:          amountCol.accType,
				Scope:         scope,
				Amount:        amount,
				Used:          maxFloat(0, amount-remaining),
				Remaining:     remaining,
			})
		}
	}
	return out
}

func buildMatrix(rows [][]string) []eligibility.NetworkMatrixRow {
	headers, start := findHeader(rows)
	if len(headers) == 0 {
		return nil
	}
	idx := headerIndex(headers)
	serviceIdx := firstIndex(idx, "service type", "service")
	participationIdx := firstIndex(idx, "participation")
	networkIdx := networkTierIndex(idx)
	percentIdx := firstIndex(idx, "percentage", "percent")
	messageIdx := firstIndex(idx, "message")
	if serviceIdx < 0 {
		return nil
	}
	byService := map[string]map[string]string{}
	if percentIdx < 0 {
		tierCols := coinsuranceTierPercentColumns(idx)
		for _, row := range rows[start:] {
			service := cell(row, serviceIdx)
			if service == "" || isCDTCode(service) {
				continue
			}
			for tier, col := range tierCols {
				percent := cell(row, col)
				if percent == "" {
					continue
				}
				if byService[service] == nil {
					byService[service] = map[string]string{}
				}
				if _, exists := byService[service][tier]; !exists {
					byService[service][tier] = percent
				}
			}
		}
	} else {
		isDeltaMichigan := participationIdx >= 0 && firstIndex(idx, "network") >= 0 && messageIdx < 0
		for _, row := range rows[start:] {
			service := cell(row, serviceIdx)
			if isCDTCode(service) {
				continue
			}
			percent := cell(row, percentIdx)
			if service == "" || percent == "" {
				continue
			}
			tier := networkID(firstNonEmpty(cell(row, participationIdx), cell(row, networkIdx)))
			if byService[service] == nil {
				byService[service] = map[string]string{}
			}
			// Keep the first (primary) percentage per service+tier; subsequent rows are code-specific exceptions.
			if _, exists := byService[service][tier]; !exists {
				byService[service][tier] = matrixPercentDisplay(percent, isDeltaMichigan)
			}
		}
	}
	var out []eligibility.NetworkMatrixRow
	for service, values := range byService {
		out = append(out, eligibility.NetworkMatrixRow{Name: service, Values: values})
	}
	return out
}

func matrixPercentDisplay(percent string, singleIsPatientResponsibility bool) string {
	percent = strings.TrimSpace(percent)
	if !singleIsPatientResponsibility {
		return percent
	}
	pct := parseInsurancePctFlexible(percent, true)
	if pct < 0 {
		return percent
	}
	return strconv.Itoa(pct) + "%"
}

func dedupeAccumulators(in []eligibility.Accumulator) []eligibility.Accumulator {
	if len(in) < 2 {
		return in
	}
	out := make([]eligibility.Accumulator, 0, len(in))
	seen := map[string]bool{}
	byID := map[string]int{}
	for _, acc := range in {
		id := strings.TrimSpace(acc.AccumulatorID)
		if id != "" {
			if existingIdx, ok := byID[id]; ok {
				existing := &out[existingIdx]
				if mergeAccumulator(existing, acc) {
					continue
				}
			}
		}
		key := strings.Join([]string{
			acc.Kind,
			acc.Type,
			acc.Scope,
			strings.ToLower(strings.TrimSpace(acc.Name)),
			strings.TrimSpace(accumulatorAmountKey(acc.Amount)),
			strings.TrimSpace(accumulatorAmountKey(acc.Used)),
			strings.TrimSpace(accumulatorAmountKey(acc.Remaining)),
		}, "|")
		if seen[key] {
			continue
		}
		seen[key] = true
		if id != "" {
			byID[id] = len(out)
		}
		out = append(out, acc)
	}
	return out
}

func mergeAccumulator(existing *eligibility.Accumulator, incoming eligibility.Accumulator) bool {
	if existing == nil {
		return false
	}
	if !strings.EqualFold(existing.Kind, incoming.Kind) ||
		!strings.EqualFold(existing.Type, incoming.Type) ||
		!strings.EqualFold(existing.Scope, incoming.Scope) {
		return false
	}
	if existing.Amount == incoming.Amount && existing.Used == incoming.Used && existing.Remaining == incoming.Remaining {
		return true
	}
	// ClaimConnect sometimes emits "Benefit Maximum" and "Benefit Remaining" as
	// separate rows with the same logical ID. Keep the larger maximum amount and
	// fold the smaller value into Remaining.
	if strings.EqualFold(existing.Kind, "maximum") {
		switch {
		case existing.Amount > incoming.Amount && incoming.Amount == incoming.Remaining:
			existing.Remaining = incoming.Amount
			existing.Used = maxFloat(0, existing.Amount-existing.Remaining)
			return true
		case incoming.Amount > existing.Amount && existing.Amount == existing.Remaining:
			incoming.Remaining = existing.Amount
			incoming.Used = maxFloat(0, incoming.Amount-incoming.Remaining)
			*existing = incoming
			return true
		}
	}
	// Keep real deductible rows ahead of zero-dollar "does not apply" rows when
	// the payer gives them the same logical bucket.
	if strings.EqualFold(existing.Kind, "deductible") {
		if existing.Amount > 0 && incoming.Amount == 0 && incoming.Remaining == 0 {
			return true
		}
		if incoming.Amount > 0 && existing.Amount == 0 && existing.Remaining == 0 {
			*existing = incoming
			return true
		}
	}
	return false
}

func accumulatorAmountKey(value float64) string {
	return strconv.FormatFloat(value, 'f', 2, 64)
}

func dedupeMatrixRows(in []eligibility.NetworkMatrixRow) []eligibility.NetworkMatrixRow {
	if len(in) < 2 {
		return in
	}
	out := make([]eligibility.NetworkMatrixRow, 0, len(in))
	seen := map[string]bool{}
	for _, row := range in {
		name := strings.TrimSpace(row.Name)
		if name == "" || isCDTCode(name) {
			continue
		}
		keyParts := []string{strings.ToLower(name)}
		for _, tier := range []string{"in", "out"} {
			keyParts = append(keyParts, tier+"="+strings.TrimSpace(row.Values[tier]))
		}
		key := strings.Join(keyParts, "|")
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, row)
	}
	return out
}

func isCDTCode(value string) bool {
	value = strings.TrimSpace(value)
	return cdtCodeRE.MatchString(value) && len(value) == 5
}

var cdtCodeRE = regexp.MustCompile(`\b[Dd]\d{4}\b`)

// buildCoverageCategories parses co-insurance/service-benefit tables and returns
// Coverage.Categories populated with CDT codes and payer coverage percentages.
// ClaimConnect has two common shapes:
//   - service/category rows where the Message column lists CDT codes
//   - procedure-code rows where the service/procedure column itself is D0120, etc.
//
// Only in-network rows are used since the default tier is always in-network for DentalXchange.
func buildCoverageCategories(rows [][]string) []eligibility.CoverageCategory {
	headers, start := findHeader(rows)
	if len(headers) == 0 {
		return nil
	}
	idx := headerIndex(headers)
	serviceIdx := firstIndex(idx, "procedure code", "service type", "service", "type")
	networkIdx := networkTierIndex(idx)
	percentIdx := firstIndex(idx, "percentage", "percent")
	messageIdx := firstIndex(idx, "message")
	if serviceIdx < 0 || percentIdx < 0 {
		return nil
	}

	type codeEntry struct {
		serviceType string
		insPct      int
	}
	codeMap := map[string]codeEntry{}
	categoryOrder := []string{}
	seenCat := map[string]bool{}

	for _, row := range rows[start:] {
		service := cell(row, serviceIdx)
		percent := cell(row, percentIdx)
		network := cell(row, networkIdx)
		message := cell(row, messageIdx)
		if service == "" || percent == "" {
			continue
		}
		if networkID(network) != "in" {
			continue
		}
		insPct := parseInsurancePct(percent)
		if insPct < 0 {
			continue
		}

		codes := cdtCodeRE.FindAllString(service+" "+message, -1)
		if len(codes) == 0 {
			continue
		}
		category := service
		if isCDTCode(service) {
			category = "Procedure Code"
		}
		if !seenCat[category] {
			seenCat[category] = true
			categoryOrder = append(categoryOrder, category)
		}
		for _, code := range codes {
			code = strings.ToUpper(code)
			if _, exists := codeMap[code]; !exists {
				codeMap[code] = codeEntry{serviceType: category, insPct: insPct}
			}
		}
	}

	byCategory := map[string][]eligibility.CoverageService{}
	for code, entry := range codeMap {
		byCategory[entry.serviceType] = append(byCategory[entry.serviceType], eligibility.CoverageService{
			Code:            code,
			CoveragePercent: entry.insPct,
		})
	}

	out := make([]eligibility.CoverageCategory, 0, len(categoryOrder))
	for _, cat := range categoryOrder {
		services := byCategory[cat]
		out = append(out, eligibility.CoverageCategory{Name: cat, Services: services})
	}
	return out
}

// parseInsurancePct extracts the insurance (payer) percentage from a "Pat% / Ins%" string.
func parseInsurancePct(percent string) int {
	parts := strings.SplitN(percent, "/", 2)
	if len(parts) != 2 {
		return -1
	}
	ins := strings.TrimSpace(strings.ReplaceAll(parts[1], "%", ""))
	v, err := strconv.Atoi(ins)
	if err != nil {
		return -1
	}
	return v
}

func parseInsurancePctFlexible(percent string, singleIsPatientResponsibility bool) int {
	percent = strings.TrimSpace(percent)
	if percent == "" {
		return -1
	}
	if strings.Contains(percent, "/") {
		return parseInsurancePct(percent)
	}
	v := parseSingleInsurancePct(percent)
	if v < 0 {
		return -1
	}
	if singleIsPatientResponsibility {
		return maxInt(0, 100-v)
	}
	return v
}

func parseSingleInsurancePct(percent string) int {
	percent = strings.TrimSpace(strings.ReplaceAll(percent, "%", ""))
	if percent == "" {
		return -1
	}
	v, err := strconv.Atoi(percent)
	if err != nil {
		return -1
	}
	if v < 0 || v > 100 {
		return -1
	}
	return v
}

type coverageCollector struct {
	order []string
	seen  map[string]bool
	data  map[string]map[string]eligibility.CoverageService
}

func newCoverageCollector() *coverageCollector {
	return &coverageCollector{
		seen: map[string]bool{},
		data: map[string]map[string]eligibility.CoverageService{},
	}
}

func (c *coverageCollector) add(category string, svc eligibility.CoverageService, prefer bool) {
	category = cleanCoverageCategory(category)
	code := strings.ToUpper(strings.TrimSpace(svc.Code))
	if category == "" || code == "" {
		return
	}
	if c.data[category] == nil {
		c.data[category] = map[string]eligibility.CoverageService{}
	}
	if !c.seen[category] {
		c.seen[category] = true
		c.order = append(c.order, category)
	}
	if existing, ok := c.data[category][code]; ok && !prefer {
		if strings.TrimSpace(svc.Limitations) != "" && strings.TrimSpace(existing.Limitations) == "" {
			existing.Limitations = svc.Limitations
			c.data[category][code] = existing
			return
		}
		// Keep the stronger row when a direct CDT row already supplied details.
		if existing.CoveragePercent >= 0 || existing.Limitations != "" {
			return
		}
	}
	c.data[category][code] = svc
}

func (c *coverageCollector) categories() []eligibility.CoverageCategory {
	var out []eligibility.CoverageCategory
	for _, category := range c.order {
		servicesByCode := c.data[category]
		if len(servicesByCode) == 0 {
			continue
		}
		var services []eligibility.CoverageService
		for _, svc := range servicesByCode {
			services = append(services, svc)
		}
		out = append(out, eligibility.CoverageCategory{Name: category, Services: services})
	}
	return out
}

func cleanCoverageCategory(value string) string {
	value = strings.TrimSpace(value)
	if isCDTCode(value) || strings.EqualFold(value, "procedure code") {
		return "Procedure Code"
	}
	value = strings.TrimPrefix(value, "Service Level Benefits - ")
	value = strings.TrimSpace(value)
	return value
}

func sectionAppliesInNetwork(title string) bool {
	title = strings.ToLower(title)
	if strings.Contains(title, "out of network") && !strings.Contains(title, "in and out") {
		return false
	}
	return strings.Contains(title, "in network") || strings.Contains(title, "in and out") || !strings.Contains(title, "network")
}

func dxCategoryAlias(service string) string {
	lower := strings.ToLower(strings.TrimSpace(service))
	switch {
	case strings.Contains(lower, "exam"), strings.Contains(lower, "bitewing"), strings.Contains(lower, "x ray"), strings.Contains(lower, "x-ray"),
		strings.Contains(lower, "prophylaxis"), strings.Contains(lower, "fluoride"), strings.Contains(lower, "sealant"), strings.Contains(lower, "space maintainer"):
		return "Preventative"
	case strings.Contains(lower, "filling"), strings.Contains(lower, "endodont"), strings.Contains(lower, "periodont"),
		strings.Contains(lower, "extraction"), strings.Contains(lower, "oral surgery"), strings.Contains(lower, "palliative"),
		strings.Contains(lower, "consultation"), strings.Contains(lower, "anesthesia"):
		return "Basic"
	case strings.Contains(lower, "crown"), strings.Contains(lower, "prosthetic"), strings.Contains(lower, "implant"), strings.Contains(lower, "orthodont"):
		return "Major"
	default:
		return service
	}
}

func findTables(n *html.Node) []*html.Node {
	var out []*html.Node
	var walk func(*html.Node)
	walk = func(node *html.Node) {
		if node.Type == html.ElementNode && node.Data == "table" {
			out = append(out, node)
		}
		for c := node.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(n)
	return out
}

func findBenefitSections(doc *html.Node) []dxBenefitSection {
	var out []dxBenefitSection
	for _, table := range findTables(doc) {
		rows := tableRows(table)
		if len(rows) == 0 {
			continue
		}
		out = append(out, dxBenefitSection{
			Title: benefitSectionTitle(table),
			Rows:  rows,
		})
	}
	return out
}

func benefitSectionTitle(table *html.Node) string {
	for n := table.Parent; n != nil; n = n.Parent {
		if n.Type != html.ElementNode || !nodeClassContains(n, "well") {
			continue
		}
		if title := firstDescendantText(n, "legend"); title != "" {
			return title
		}
	}
	return ""
}

func firstDescendantText(n *html.Node, tag string) string {
	if n == nil {
		return ""
	}
	var result string
	var walk func(*html.Node)
	walk = func(node *html.Node) {
		if result != "" {
			return
		}
		if node.Type == html.ElementNode && strings.EqualFold(node.Data, tag) {
			result = strings.TrimSpace(nodeText(node))
			return
		}
		for c := node.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(n)
	return result
}

func nodeClassContains(n *html.Node, needle string) bool {
	for _, attr := range n.Attr {
		if strings.EqualFold(attr.Key, "class") && strings.Contains(strings.ToLower(attr.Val), strings.ToLower(needle)) {
			return true
		}
	}
	return false
}

func tableRows(table *html.Node) [][]string {
	var rows [][]string
	var walk func(*html.Node)
	walk = func(node *html.Node) {
		if node.Type == html.ElementNode && node.Data == "tr" {
			var row []string
			for c := node.FirstChild; c != nil; c = c.NextSibling {
				if c.Type == html.ElementNode && (c.Data == "td" || c.Data == "th") {
					row = append(row, nodeText(c))
				}
			}
			if len(row) > 0 {
				rows = append(rows, row)
			}
			return
		}
		for c := node.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(table)
	return rows
}

func findHeader(rows [][]string) ([]string, int) {
	for i, row := range rows {
		joined := strings.ToLower(strings.Join(row, " "))
		if strings.Contains(joined, "service type") || strings.Contains(joined, "amount") || strings.Contains(joined, "percentage") {
			return row, i + 1
		}
	}
	return nil, 0
}

func headerIndex(headers []string) map[string]int {
	out := map[string]int{}
	for i, h := range headers {
		out[strings.ToLower(strings.TrimSpace(h))] = i
	}
	return out
}

// firstIndex returns the column index of the first needle found in the header map.
// When multiple headers match the same needle it returns the lowest column index
// to avoid non-deterministic map iteration.
func firstIndex(idx map[string]int, needles ...string) int {
	for _, needle := range needles {
		result := -1
		for header, i := range idx {
			if strings.Contains(header, needle) {
				if result < 0 || i < result {
					result = i
				}
			}
		}
		if result >= 0 {
			return result
		}
	}
	return -1
}

// networkTierIndex finds a column that is a standalone network tier identifier
// (e.g. "Network", "Participation") rather than a hybrid like "In- and Out- of Network Lifetime Amount".
func networkTierIndex(idx map[string]int) int {
	for header, i := range idx {
		lower := strings.ToLower(header)
		if lower == "network" || lower == "participation" {
			return i
		}
	}
	// Fallback: a column containing "network" but not an amount/year descriptor.
	result := -1
	for header, i := range idx {
		lower := strings.ToLower(header)
		if strings.Contains(lower, "network") &&
			!strings.Contains(lower, "amount") &&
			!strings.Contains(lower, "year") &&
			!strings.Contains(lower, "remaining") {
			if result < 0 || i < result {
				result = i
			}
		}
	}
	return result
}

type accumulatorAmountColumn struct {
	amountIdx    int
	remainingIdx int
	accType      string
	network      string
}

func accumulatorAmountColumns(idx map[string]int, kind string) []accumulatorAmountColumn {
	type headerCol struct {
		header string
		index  int
	}
	var headers []headerCol
	for header, i := range idx {
		headers = append(headers, headerCol{header: strings.ToLower(strings.TrimSpace(header)), index: i})
	}
	sort.Slice(headers, func(i, j int) bool { return headers[i].index < headers[j].index })

	genericIdx := exactHeaderIndex(idx, "amount")
	remainingByBase := map[string]int{}
	remainingByNetwork := map[string]int{}
	lifetimeRemainingIdx := -1
	for _, h := range headers {
		if !strings.Contains(h.header, "remaining amount") {
			continue
		}
		if strings.Contains(h.header, "lifetime") {
			lifetimeRemainingIdx = h.index
			continue
		}
		remainingByBase[amountHeaderBase(h.header)] = h.index
		if network := networkFromHeader(h.header); network != "" {
			if _, exists := remainingByNetwork[network]; !exists {
				remainingByNetwork[network] = h.index
			}
		}
	}

	var out []accumulatorAmountColumn
	for _, h := range headers {
		if !calendarAmountHeader(h.header) {
			continue
		}
		network := networkFromHeader(h.header)
		remainingIdx, ok := remainingByBase[amountHeaderBase(h.header)]
		if !ok {
			if byNetwork, ok := remainingByNetwork[network]; ok {
				remainingIdx = byNetwork
			} else {
				remainingIdx = exactHeaderIndex(idx, "remaining amount")
			}
		}
		out = append(out, accumulatorAmountColumn{amountIdx: h.index, remainingIdx: remainingIdx, accType: "calendar", network: network})
	}
	if strings.EqualFold(kind, "maximum") {
		for _, h := range headers {
			if !lifetimeAmountHeader(h.header) {
				continue
			}
			out = append(out, accumulatorAmountColumn{amountIdx: h.index, remainingIdx: lifetimeRemainingIdx, accType: "lifetime", network: networkFromHeader(h.header)})
		}
	}
	if len(out) == 0 && genericIdx >= 0 {
		out = append(out, accumulatorAmountColumn{amountIdx: genericIdx, remainingIdx: exactHeaderIndex(idx, "remaining amount"), accType: "calendar"})
	}
	return out
}

func calendarAmountHeader(header string) bool {
	header = strings.ToLower(strings.TrimSpace(header))
	if strings.Contains(header, "remaining") || strings.Contains(header, "lifetime") {
		return false
	}
	return header == "amount" ||
		strings.Contains(header, "calendar year amount") ||
		strings.Contains(header, "service year amount") ||
		strings.Contains(header, "contract amount")
}

func lifetimeAmountHeader(header string) bool {
	header = strings.ToLower(strings.TrimSpace(header))
	return strings.Contains(header, "lifetime amount") && !strings.Contains(header, "remaining")
}

func amountHeaderBase(header string) string {
	header = strings.ToLower(strings.TrimSpace(header))
	for _, phrase := range []string{
		"calendar year amount",
		"service year amount",
		"contract amount",
		"remaining amount",
		"amount",
	} {
		header = strings.ReplaceAll(header, phrase, "")
	}
	return strings.Join(strings.Fields(header), " ")
}

func networkFromHeader(header string) string {
	lower := strings.ToLower(header)
	if strings.Contains(lower, "out-of-network") || strings.Contains(lower, "out of network") {
		return "Out-Of-Network"
	}
	if strings.Contains(lower, "in-network") || strings.Contains(lower, "in network") {
		return "In-Network"
	}
	return ""
}

func exactHeaderIndex(idx map[string]int, headers ...string) int {
	for _, want := range headers {
		if i, ok := idx[strings.ToLower(strings.TrimSpace(want))]; ok {
			return i
		}
	}
	return -1
}

func coinsuranceTierPercentColumns(idx map[string]int) map[string]int {
	out := map[string]int{}
	for header, i := range idx {
		lower := strings.ToLower(strings.TrimSpace(header))
		lower = strings.ReplaceAll(lower, "_", " ")
		lower = strings.Join(strings.Fields(lower), " ")
		switch lower {
		case "in-network", "in network":
			out["in"] = i
		case "out-of-network", "out of network":
			out["out"] = i
		}
	}
	return out
}

// splitCoverageService handles the DentalXchange "Individual - Service Name" / "Family - Service Name"
// cell format used in the Deductibles table, splitting the scope prefix from the service name.
func splitCoverageService(value string) (scope, service string) {
	if idx := strings.Index(value, " - "); idx >= 0 {
		prefix := strings.ToLower(strings.TrimSpace(value[:idx]))
		if prefix == "individual" || prefix == "family" {
			return prefix, strings.TrimSpace(value[idx+3:])
		}
	}
	return "", value
}

func labelValue(text, label string) string {
	text = strings.Join(strings.Fields(text), " ")
	if text == "" {
		return ""
	}
	quoted := regexp.QuoteMeta(label)
	re := regexp.MustCompile(`(?i)` + quoted + `\s*:?\s*([^:]{1,120}?)(?:\s+[A-Z][A-Za-z/#() ]{2,35}\s*:|$)`)
	if m := re.FindStringSubmatch(text); len(m) == 2 {
		return strings.TrimSpace(m[1])
	}
	return ""
}

func nodeText(n *html.Node) string {
	var sb strings.Builder
	var walk func(*html.Node)
	walk = func(node *html.Node) {
		if node.Type == html.ElementNode {
			switch strings.ToLower(node.Data) {
			case "script", "style", "noscript":
				return
			}
		}
		if node.Type == html.TextNode {
			sb.WriteString(node.Data)
			sb.WriteByte(' ')
		}
		for c := node.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(n)
	return strings.Join(strings.Fields(html.UnescapeString(sb.String())), " ")
}

func payerLabelName(label string) string {
	label = strings.TrimSpace(label)
	if label == "" {
		return ""
	}
	parts := strings.Split(label, " - ")
	if len(parts) > 1 {
		last := strings.TrimSpace(parts[len(parts)-1])
		if _, err := strconv.Atoi(last); err == nil {
			return strings.TrimSpace(strings.Join(parts[:len(parts)-1], " - "))
		}
	}
	return label
}

func cleanDXValue(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	lower := strings.ToLower(value)
	if strings.Contains(lower, "<![cdata") || strings.Contains(value, "{") || strings.Contains(value, "}") {
		return ""
	}
	labelCount := 0
	for _, marker := range []string{"name:", "plan type:", "description:", "group#:", "group name:", "customer service:", "comments:"} {
		if strings.Contains(lower, marker) {
			labelCount++
		}
	}
	if labelCount >= 3 {
		return ""
	}
	return value
}

func flattenRows(rows [][]string) string {
	var parts []string
	for _, row := range rows {
		parts = append(parts, row...)
	}
	return strings.Join(parts, " ")
}

func cell(row []string, idx int) string {
	if idx < 0 || idx >= len(row) {
		return ""
	}
	return strings.TrimSpace(row[idx])
}

func parseMoney(value string) float64 {
	value = strings.ReplaceAll(value, "$", "")
	value = strings.ReplaceAll(value, ",", "")
	value = strings.TrimSpace(value)
	if value == "" {
		return -1
	}
	f, err := strconv.ParseFloat(value, 64)
	if err != nil {
		return -1
	}
	return f
}

func hasMoney(row []string) bool {
	for _, value := range row {
		if parseMoney(value) >= 0 && strings.Contains(value, "$") {
			return true
		}
	}
	return false
}

func scopeFromText(value string) string {
	lower := strings.ToLower(value)
	if strings.Contains(lower, "family") {
		return "family"
	}
	return "individual"
}

func networkID(value string) string {
	lower := strings.ToLower(value)
	if strings.Contains(lower, "out") {
		return "out"
	}
	return "in"
}

func accumulatorName(kind, accType, scope, network, service string) string {
	parts := []string{}
	if service != "" {
		parts = append(parts, service)
	}
	parts = append(parts, strings.Title(scope), strings.Title(accType), strings.Title(kind))
	if networkID(network) == "out" {
		parts = append(parts, "(OON)")
	}
	return strings.Join(parts, " ")
}

func defaultNetworkTiers() []eligibility.NetworkTier {
	return []eligibility.NetworkTier{
		{TierID: "in", DisplayName: "In-Network", IsContracted: true},
		{TierID: "out", DisplayName: "Out-of-Network", IsContracted: false},
	}
}

func normalizeDate(value string) string {
	value = strings.TrimSpace(value)
	layouts := []string{"01/02/2006", "1/2/2006", "01-02-2006", "2006-01-02"}
	for _, layout := range layouts {
		if t, err := time.Parse(layout, value); err == nil {
			return t.Format("2006-01-02")
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

func slug(value string) string {
	value = strings.ToLower(value)
	var b strings.Builder
	for _, r := range value {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			b.WriteRune(r)
		} else if b.Len() > 0 {
			b.WriteByte('-')
		}
	}
	return strings.Trim(b.String(), "-")
}

var accVisitPattern = regexp.MustCompile(`(?i)\b(\d+)\s+Visits?\b`)

// parseAccumulatorNote builds a human-readable note from the message and delivery
// pattern columns of a DXC Limitations/Maximums table row.
// It extracts "COMBINED WITH …" shared-pool text and visit-frequency limits.
// Returns "" when the message contains only D-codes or plan-variant tags.
func parseAccumulatorNote(message, delivery string) string {
	message = strings.TrimSpace(message)
	delivery = strings.TrimSpace(delivery)
	if message == "" && delivery == "" {
		return ""
	}
	upper := strings.ToUpper(message)
	var parts []string

	// Extract shared-pool text: "COMBINED WITH A, B, C ADVANTAGE AND TOTAL" → "Shared with: A, B, C"
	if i := strings.Index(upper, "COMBINED WITH "); i >= 0 {
		shared := message[i+len("COMBINED WITH "):]
		// Trim known stop phrases that follow the service list.
		for _, stop := range []string{"ADVANTAGE AND TOTAL", "ADVANTAGE ONLY", "TOTAL ONLY", "ADVANTAGE", "TOTAL"} {
			if j := strings.Index(strings.ToUpper(shared), stop); j >= 0 {
				shared = shared[:j]
			}
		}
		shared = strings.TrimSpace(shared)
		if shared != "" {
			// Title-case for readability (message text is typically all-caps).
			words := strings.Fields(strings.ToLower(shared))
			for k, w := range words {
				if len(w) > 0 {
					words[k] = strings.ToUpper(w[:1]) + w[1:]
				}
			}
			parts = append(parts, "Shared with: "+strings.Join(words, " "))
		}
	}

	// Extract visit count from message ("1 Visit", "2 Visits"), combined with delivery pattern.
	if m := accVisitPattern.FindString(message); m != "" {
		freq := strings.TrimSpace(m)
		if delivery != "" {
			freq += " " + delivery
		}
		parts = append(parts, freq)
	} else if delivery != "" {
		parts = append(parts, delivery)
	}

	return strings.Join(parts, " · ")
}

// extractDXPlanName resolves the plan design name from benPairs with three strategies:
//  1. "description" / "plan type" keys — used by Aetna, Ameritas, Delta, Cigna
//  2. "plan name" key — only when the value contains plan-design keywords (not an employer name)
//  3. "group name" key — fallback for Principal, where the plan design lives under "Group Name"
//
// Values longer than 60 chars are rejected as limitation/notice text.
func extractDXPlanName(benPairs map[string]string, pageText string) string {
	hasPlanKeyword := func(v string) bool {
		lower := strings.ToLower(v)
		for _, kw := range []string{"ppo", "hmo", "dental", "dmo", "dhmo", "indemnity", "advantage"} {
			if strings.Contains(lower, kw) {
				return true
			}
		}
		return false
	}
	for _, key := range []string{"description", "plan type"} {
		if v := cleanDXValue(tablePairGet(benPairs, key)); v != "" && len(v) < 60 {
			return v
		}
	}
	if v := cleanDXValue(tablePairGet(benPairs, "plan name")); v != "" && len(v) < 60 && hasPlanKeyword(v) {
		return v
	}
	if v := cleanDXValue(tablePairGet(benPairs, "group name")); v != "" && len(v) < 80 && hasPlanKeyword(v) {
		return v
	}
	return cleanDXValue(labelValue(pageText, "Plan Type"))
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func maxFloat(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func joinNonEmpty(values ...string) string {
	var parts []string
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" {
			parts = append(parts, value)
		}
	}
	return strings.Join(parts, " ")
}
