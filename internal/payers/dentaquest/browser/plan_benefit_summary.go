package browser

import (
	"fmt"
	"log"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"insurance-benefit-agent-go/internal/logging"
	"insurance-benefit-agent-go/internal/payers/dentaquest/eligibility"
)

var dentaquestNetworkTiers = []eligibility.NetworkTier{
	{TierID: "in_network", DisplayName: "In Network", IsContracted: true},
	{TierID: "out_network", DisplayName: "Out of Network", IsContracted: false},
}

// applyPlanBenefitSummaryNetworkMatrix reads the plan-benefit-summary XHR
// payload and populates el.NetworkTiers and el.NetworkMatrix.
func applyPlanBenefitSummaryNetworkMatrix(s *Session, el *eligibility.PatientEligibility) bool {
	stored := s.GetPayload("plan-benefit-summary")
	if stored == nil {
		return false
	}
	p := asStringMap(stored.Payload)
	if p == nil {
		return false
	}

	items := asSlice(p["benefitSummaryItems"])
	if len(items) == 0 {
		return false
	}

	matrix := buildNetworkMatrix(items)
	if len(matrix) == 0 {
		return false
	}

	el.NetworkTiers = dentaquestNetworkTiers
	el.NetworkMatrix = matrix
	log.Printf("[DentaQuest] network matrix: %d category row(s)", len(matrix))
	logging.Info("dentaquest.browser", "dentaquest.member.network_matrix.built", "built network matrix from plan benefit summary", map[string]any{
		"rows": len(matrix),
	})
	return true
}

// ── matrix builder ────────────────────────────────────────────────────────────

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

// ── helpers ───────────────────────────────────────────────────────────────────

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
