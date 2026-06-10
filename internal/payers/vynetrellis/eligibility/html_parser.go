package eligibility

import (
	"strconv"
	"strings"

	"golang.org/x/net/html"

	"insurance-benefit-agent-go/internal/eligibility"
)

// enrichFromHTML parses the Vyne Trellis HtmlResult and adds accumulators,
// eligibility dates, group info, and plan name to el in-place.
func enrichFromHTML(el *eligibility.PatientEligibility, htmlContent string) {
	if el == nil || htmlContent == "" {
		return
	}
	doc, err := html.Parse(strings.NewReader(htmlContent))
	if err != nil {
		return
	}
	e := &htmlEnricher{el: el}
	e.walkTables(doc)
}

type htmlEnricher struct {
	el *eligibility.PatientEligibility
}

func (e *htmlEnricher) walkTables(n *html.Node) {
	if n.Type == html.ElementNode && n.Data == "table" {
		e.processTable(n)
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		e.walkTables(c)
	}
}

func (e *htmlEnricher) processTable(table *html.Node) {
	cls := nodeAttr(table, "class")
	switch {
	case strings.Contains(cls, "eb-detail"):
		e.processEbDetail(table)
	case strings.Contains(cls, "elig-date"):
		e.processEligDate(table)
	case strings.Contains(cls, "response-detail"):
		e.processResponseDetail(table)
	}
}

// ── Eligibility dates ─────────────────────────────────────────────────────

func (e *htmlEnricher) processEligDate(table *html.Node) {
	eachTR(table, func(tr *html.Node) {
		if !nodeHasClass(tr, "payer-detail") {
			return
		}
		cells := directChildren(tr, "td")
		if len(cells) < 2 {
			return
		}
		label := nodeText(cells[0])
		val := nodeText(cells[1])
		switch label {
		case "Eligibility Begin Date":
			if e.el.Patient.EligibilityEffectiveDate == "" {
				e.el.Patient.EligibilityEffectiveDate = normalizeDOB(val)
			}
		case "Eligibility End Date":
			if e.el.Patient.EligibilityEndDate == "" {
				e.el.Patient.EligibilityEndDate = normalizeDOB(val)
			}
		case "Plan Date(s)":
			// Format: "01/01/2026-12/31/2026"
			if e.el.Patient.EligibilityEffectiveDate == "" {
				parts := strings.SplitN(val, "-", 2)
				if len(parts) == 2 {
					e.el.Patient.EligibilityEffectiveDate = normalizeDOB(strings.TrimSpace(parts[0]))
					e.el.Patient.EligibilityEndDate = normalizeDOB(strings.TrimSpace(parts[1]))
				}
			}
		}
	})
}

// ── response-detail sections (Payer, Subscriber, Dependent) ──────────────

func (e *htmlEnricher) processResponseDetail(table *html.Node) {
	headerText := ""
	eachTR(table, func(tr *html.Node) {
		if nodeHasClass(tr, "response-detail-header") && headerText == "" {
			headerText = nodeText(tr)
		}
	})
	switch {
	case strings.HasPrefix(headerText, "Payer"):
		e.parsePayer(table)
	case strings.HasPrefix(headerText, "Subscriber"), strings.HasPrefix(headerText, "Dependent"):
		e.parseGroupNumber(table)
	}
}

func (e *htmlEnricher) parsePayer(table *html.Node) {
	if e.el.Plan.PlanName != "" {
		return
	}
	// payer-info th contains "CARRIER NAME (ID)" — already set as Carrier from probe bundle.
	// Nothing more to do here; plan name comes from Active Coverage section.
}

func (e *htmlEnricher) parseGroupNumber(table *html.Node) {
	eachTR(table, func(tr *html.Node) {
		cells := directChildren(tr, "td")
		if len(cells) < 2 {
			return
		}
		if !strings.Contains(nodeText(cells[0]), "Group Number") {
			return
		}
		groupStr := nodeText(cells[1])
		if idx := strings.Index(groupStr, " - "); idx >= 0 {
			num := strings.TrimSpace(groupStr[:idx])
			name := strings.TrimSpace(groupStr[idx+3:])
			if e.el.Patient.GroupNumber == "" || e.el.Patient.GroupNumber == "99999" {
				e.el.Patient.GroupNumber = num
			}
			if e.el.Plan.GroupName == "" {
				e.el.Plan.GroupName = name
			}
		} else if e.el.Patient.GroupNumber == "" {
			e.el.Patient.GroupNumber = strings.TrimSpace(groupStr)
		}
	})
}

// ── eb-detail sections (Active Coverage, Limitations) ─────────────────────

func (e *htmlEnricher) processEbDetail(table *html.Node) {
	headerText := ""
	eachTR(table, func(tr *html.Node) {
		if nodeHasClass(tr, "eb-detail-header") && headerText == "" {
			headerText = nodeText(tr)
		}
	})
	switch {
	case strings.Contains(headerText, "Active Coverage"):
		e.parseActiveCoverage(table)
	case strings.Contains(headerText, "Limitations"):
		e.parseLimitations(table)
	}
}

func (e *htmlEnricher) parseActiveCoverage(table *html.Node) {
	if e.el.Plan.PlanName != "" {
		return
	}
	carrierLower := strings.ToLower(e.el.Plan.Carrier)
	eachDescendantByClass(table, "tr", "no-indent", func(tr *html.Node) {
		cells := directChildren(tr, "td")
		if len(cells) < 4 {
			return
		}
		desc := strings.TrimSpace(nodeText(cells[3]))
		if desc == "" {
			return
		}
		descLower := strings.ToLower(desc)
		// Skip if description is just the carrier name
		if strings.Contains(carrierLower, descLower) {
			return
		}
		e.el.Plan.PlanName = desc
	})
}

// ── Limitations → Accumulators ────────────────────────────────────────────

type limitRow struct {
	serviceType string
	level       string
	amount      float64
	timePeriod  string
	network     string
}

func (e *htmlEnricher) parseLimitations(table *html.Node) {
	var rows []limitRow

	eachDescendantByClass(table, "tr", "service-type-procedure-detail-row", func(detailRow *html.Node) {
		noIndent := findDescendantByClass(detailRow, "tr", "no-indent")
		if noIndent == nil {
			return
		}
		cells := directChildren(noIndent, "td")
		if len(cells) < 6 {
			return
		}
		svcType := strings.TrimSpace(nodeText(cells[0]))
		level := strings.TrimSpace(nodeText(cells[1]))
		amtStr := strings.TrimSpace(nodeText(cells[2]))
		period := strings.TrimSpace(nodeText(cells[3]))
		network := strings.TrimSpace(nodeText(cells[5]))

		if amtStr == "" || period == "" {
			return
		}
		amt, err := parseDollarAmount(amtStr)
		if err != nil || amt == 0 {
			return
		}
		rows = append(rows, limitRow{
			serviceType: svcType,
			level:       level,
			amount:      amt,
			timePeriod:  period,
			network:     network,
		})
	})

	e.el.Accumulators = append(e.el.Accumulators, buildAccumulators(rows)...)
}

type accKey struct {
	serviceType string
	level       string
	network     string
	kind        string // "calendar" or "lifetime"
}

func buildAccumulators(rows []limitRow) []eligibility.Accumulator {
	maxAmts := map[accKey]float64{}
	remAmts := map[accKey]float64{}
	var order []accKey

	for _, row := range rows {
		period := strings.ToLower(strings.TrimSpace(row.timePeriod))
		var kind string
		var isRem bool

		switch period {
		case "calendar year":
			kind = "calendar"
		case "remaining":
			kind, isRem = "calendar", true
		case "lifetime":
			kind = "lifetime"
		case "lifetime remaining":
			kind, isRem = "lifetime", true
		default:
			continue
		}

		k := accKey{
			serviceType: strings.ToLower(row.serviceType),
			level:       strings.ToLower(row.level),
			network:     strings.ToLower(row.network),
			kind:        kind,
		}
		if isRem {
			remAmts[k] = row.amount
		} else {
			if _, exists := maxAmts[k]; !exists {
				order = append(order, k)
			}
			maxAmts[k] = row.amount
		}
	}

	seen := map[accKey]bool{}
	var result []eligibility.Accumulator
	for _, k := range order {
		if seen[k] {
			continue
		}
		seen[k] = true

		maxAmt := maxAmts[k]
		remAmt, hasRem := remAmts[k]
		if !hasRem {
			remAmt = maxAmt
		}
		used := maxAmt - remAmt
		if used < 0 {
			used = 0
		}

		result = append(result, eligibility.Accumulator{
			AccumulatorID: accID(k),
			Name:          accName(k),
			Kind:          "maximum",
			Type:          k.kind,
			Scope:         scopeFromLevel(k.level),
			Amount:        maxAmt,
			Used:          used,
			Remaining:     remAmt,
		})
	}
	return result
}

func accID(k accKey) string {
	net := "in"
	if strings.Contains(k.network, "out") {
		net = "out"
	}
	return strings.Join([]string{k.serviceType, k.level, k.kind, net}, "_")
}

func accName(k accKey) string {
	svc := k.serviceType
	if svc == "" {
		svc = "dental care"
	}
	scope := scopeFromLevel(k.level)
	var kindLabel string
	if k.kind == "lifetime" {
		kindLabel = "Lifetime Maximum"
	} else {
		kindLabel = "Calendar Year Maximum"
	}
	var netLabel string
	if strings.Contains(k.network, "out") {
		netLabel = " (OON)"
	}
	parts := []string{titleCase(svc)}
	if scope != "" {
		parts = append(parts, titleCase(scope))
	}
	parts = append(parts, kindLabel)
	return strings.Join(parts, " ") + netLabel
}

func scopeFromLevel(level string) string {
	switch strings.ToLower(level) {
	case "individual":
		return "individual"
	case "family":
		return "family"
	default:
		return ""
	}
}

// ── HTML helpers ──────────────────────────────────────────────────────────

func nodeAttr(n *html.Node, key string) string {
	for _, a := range n.Attr {
		if a.Key == key {
			return a.Val
		}
	}
	return ""
}

func nodeHasClass(n *html.Node, class string) bool {
	for _, c := range strings.Fields(nodeAttr(n, "class")) {
		if c == class {
			return true
		}
	}
	return false
}

func nodeText(n *html.Node) string {
	var sb strings.Builder
	var walk func(*html.Node)
	walk = func(node *html.Node) {
		if node.Type == html.TextNode {
			sb.WriteString(node.Data)
		}
		for c := node.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(n)
	return strings.TrimSpace(sb.String())
}

// directChildren returns immediate child elements with the given tag.
func directChildren(parent *html.Node, tag string) []*html.Node {
	var result []*html.Node
	for c := parent.FirstChild; c != nil; c = c.NextSibling {
		if c.Type == html.ElementNode && c.Data == tag {
			result = append(result, c)
		}
	}
	return result
}

// eachTR calls fn for each <tr> in the subtree, stopping descent into found <tr>s
// so nested-table rows are not included.
func eachTR(n *html.Node, fn func(*html.Node)) {
	var walk func(*html.Node)
	walk = func(node *html.Node) {
		if node.Type == html.ElementNode && node.Data == "tr" {
			fn(node)
			return
		}
		for c := node.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(n)
}

// eachDescendantByClass calls fn for each element with the given tag and class,
// stopping descent into matched elements.
func eachDescendantByClass(n *html.Node, tag, class string, fn func(*html.Node)) {
	var walk func(*html.Node)
	walk = func(node *html.Node) {
		if node.Type == html.ElementNode && node.Data == tag && nodeHasClass(node, class) {
			fn(node)
			return
		}
		for c := node.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(n)
}

// findDescendantByClass returns the first descendant element with the given tag and class.
func findDescendantByClass(n *html.Node, tag, class string) *html.Node {
	var result *html.Node
	var walk func(*html.Node)
	walk = func(node *html.Node) {
		if result != nil {
			return
		}
		if node.Type == html.ElementNode && node.Data == tag && nodeHasClass(node, class) {
			result = node
			return
		}
		for c := node.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(n)
	return result
}

func titleCase(s string) string {
	if s == "" {
		return s
	}
	words := strings.Fields(s)
	for i, w := range words {
		if len(w) > 0 {
			words[i] = strings.ToUpper(w[:1]) + strings.ToLower(w[1:])
		}
	}
	return strings.Join(words, " ")
}

func parseDollarAmount(s string) (float64, error) {
	s = strings.ReplaceAll(s, "$", "")
	s = strings.ReplaceAll(s, ",", "")
	s = strings.TrimSpace(s)
	return strconv.ParseFloat(s, 64)
}
