// Package scrapeutil provides string and type-assertion helpers shared across
// payer scraper adapters.
package scrapeutil

import (
	"fmt"
	"regexp"
	"strings"
)

var (
	reNonDigit = regexp.MustCompile(`\D`)
	reNonAlnum = regexp.MustCompile(`[^a-z0-9]+`)
	reMoney    = regexp.MustCompile(`[$,]`)
)

// NormalizeSpace collapses all whitespace runs to a single space and trims.
func NormalizeSpace(value string) string {
	return strings.Join(strings.Fields(value), " ")
}

// NormalizeDigits strips every non-digit character from value.
func NormalizeDigits(value string) string {
	return reNonDigit.ReplaceAllString(value, "")
}

// NormalizeDateDigits returns an 8-digit date string in MMDDYYYY order
// regardless of whether the input was YYYYMMDD or MMDDYYYY.
// Returns "" if the input does not contain exactly 8 digits.
func NormalizeDateDigits(value string) string {
	digits := NormalizeDigits(value)
	if len(digits) != 8 {
		return ""
	}
	if digits[:4] > "1900" {
		// YYYYMMDD → MMDDYYYY
		return digits[4:6] + digits[6:8] + digits[:4]
	}
	return digits
}

// ParseMoney parses a dollar-amount string (e.g. "$1,234.56") into float64.
func ParseMoney(value string) float64 {
	cleaned := reMoney.ReplaceAllString(NormalizeSpace(value), "")
	var f float64
	if _, err := fmt.Sscanf(strings.TrimSpace(cleaned), "%f", &f); err != nil {
		return 0
	}
	return f
}

// ToSlug converts a human-readable label to a lowercase hyphenated slug.
func ToSlug(value string) string {
	normalized := strings.ToLower(NormalizeSpace(value))
	return strings.Trim(reNonAlnum.ReplaceAllString(normalized, "-"), "-")
}

// AsStringMap safely type-asserts any to map[string]any, returning nil on failure.
func AsStringMap(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

// AsSlice safely type-asserts any to []any, returning nil on failure.
func AsSlice(v any) []any {
	s, _ := v.([]any)
	return s
}

// AnyStr returns the first non-empty string found for any of the given keys in
// m. Values are NormalizeSpace'd before the empty-check.
func AnyStr(m map[string]any, keys ...string) string {
	for _, key := range keys {
		if v, ok := m[key]; ok && v != nil {
			if s := NormalizeSpace(fmt.Sprint(v)); s != "" {
				return s
			}
		}
	}
	return ""
}
