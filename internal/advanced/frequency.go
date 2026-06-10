package advanced

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// ParseFrequency attempts to extract a structured frequency limit from a
// dental benefit limitation string.  Returns nil when no frequency pattern
// is recognised so callers can distinguish "no limit" from "unparseable".
//
// The returned CodeFrequency has Allowed, Period, PeriodType, PeriodMonths, and
// Scope populated.  The computed fields (Used, Remaining, Exceeded, etc.) are
// filled separately by the builder after cross-referencing treatment history.
func ParseFrequency(text string) *CodeFrequency {
	if strings.TrimSpace(text) == "" {
		return nil
	}
	t := strings.ToLower(strings.TrimSpace(text))

	count := extractCount(t)
	scope := extractScope(t)

	// ── lifetime ──────────────────────────────────────────────────────────────
	if strings.Contains(t, "per lifetime") || strings.Contains(t, "per patient lifetime") {
		allowed := count
		return &CodeFrequency{
			Allowed:      &allowed,
			Period:       "lifetime",
			PeriodType:   "lifetime",
			PeriodMonths: 0,
			Scope:        scope,
			Summary:      fmt.Sprintf("%s per lifetime %s", pluralize(count), scope),
		}
	}

	// ── calendar year ─────────────────────────────────────────────────────────
	if strings.Contains(t, "calendar year") {
		allowed := count
		return &CodeFrequency{
			Allowed:      &allowed,
			Period:       "1 year",
			PeriodType:   "calendar",
			PeriodMonths: 12,
			Scope:        scope,
			Summary:      fmt.Sprintf("%s per calendar year %s", pluralize(count), scope),
		}
	}

	// ── within N years ────────────────────────────────────────────────────────
	if m := withinNYearsRe.FindStringSubmatch(t); m != nil {
		n, _ := strconv.Atoi(m[1])
		months := n * 12
		period := fmt.Sprintf("%d %s", n, pluralUnit(n, "year"))
		allowed := count
		return &CodeFrequency{
			Allowed:      &allowed,
			Period:       period,
			PeriodType:   "rolling",
			PeriodMonths: months,
			Scope:        scope,
			Summary:      fmt.Sprintf("%s within %s %s", pluralize(count), period, scope),
		}
	}

	// ── within N months ───────────────────────────────────────────────────────
	if m := withinNMonthsRe.FindStringSubmatch(t); m != nil {
		n, _ := strconv.Atoi(m[1])
		period := fmt.Sprintf("%d %s", n, pluralUnit(n, "month"))
		allowed := count
		return &CodeFrequency{
			Allowed:      &allowed,
			Period:       period,
			PeriodType:   "rolling",
			PeriodMonths: n,
			Scope:        scope,
			Summary:      fmt.Sprintf("%s within %s %s", pluralize(count), period, scope),
		}
	}

	return nil
}

// extractCount picks the most specific count word from a limitation string,
// preferring the primary benefit sentence over follow-up qualifiers.
func extractCount(t string) int {
	// "limited to N" / "benefit is limited to any N"
	if m := limitedToRe.FindStringSubmatch(t); m != nil {
		return parseCountWord(m[1])
	}
	// First count word anywhere in text.
	if m := firstCountRe.FindStringSubmatch(t); m != nil {
		return parseCountWord(m[1])
	}
	return 1
}

func parseCountWord(w string) int {
	switch strings.ToLower(strings.TrimSpace(w)) {
	case "once", "one":
		return 1
	case "twice", "two":
		return 2
	case "three":
		return 3
	case "four":
		return 4
	}
	n, err := strconv.Atoi(w)
	if err != nil || n < 1 {
		return 1
	}
	return n
}

func extractScope(t string) string {
	switch {
	case strings.Contains(t, "per tooth"):
		return "per tooth"
	case strings.Contains(t, "per quadrant"):
		return "per quadrant"
	case strings.Contains(t, "per arch"):
		return "per arch"
	case strings.Contains(t, "per sextant"):
		return "per sextant"
	default:
		return "per patient"
	}
}

func pluralize(count int) string {
	if count == 1 {
		return "once"
	}
	return strconv.Itoa(count) + " times"
}

func pluralUnit(n int, unit string) string {
	if n == 1 {
		return unit
	}
	return unit + "s"
}

// Compiled regexes.
var (
	withinNYearsRe = regexp.MustCompile(`within a[n]?\s+(\d+)\s+year`)
	withinNMonthsRe = regexp.MustCompile(`within a[n]?\s+(\d+)\s+month`)

	limitedToRe = regexp.MustCompile(
		`(?:limited to|benefit is limited to)\s+(?:any\s+)?(once|twice|one|two|three|four|\d+)\b`,
	)
	firstCountRe = regexp.MustCompile(
		`\b(once|twice|one|two|three|four|\d+)\b`,
	)
)

// FrequencyFamilyRules maps limitation text patterns to the family of CDT codes
// that share the same frequency counter.  When a limitation matches, usage is
// counted across all codes in the family, not just the requested code.
var FrequencyFamilyRules = []struct {
	Pattern *regexp.Regexp
	Codes   []string
}{
	{
		Pattern: regexp.MustCompile(`(?i)oral evaluation procedure`),
		Codes:   []string{"D0120", "D0140", "D0150", "D0160", "D0170", "D0180"},
	},
	{
		Pattern: regexp.MustCompile(`(?i)prophylaxis procedures`),
		Codes:   []string{"D1110", "D1120", "D4346", "D4355", "D4910"},
	},
	{
		Pattern: regexp.MustCompile(`(?i)fluoride procedures`),
		Codes:   []string{"D1206", "D1208"},
	},
	{
		Pattern: regexp.MustCompile(`(?i)bitewing x-?ray procedure`),
		Codes:   []string{"D0270", "D0272", "D0273", "D0274", "D0277"},
	},
	{
		Pattern: regexp.MustCompile(`(?i)either one \(d0210\).*?\(d0330\)`),
		Codes:   []string{"D0210", "D0330"},
	},
}
