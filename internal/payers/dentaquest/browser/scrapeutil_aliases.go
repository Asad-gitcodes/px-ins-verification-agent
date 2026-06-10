package browser

// scrapeutil_aliases.go re-exports the shared scrapeutil helpers as
// package-local names so the rest of this package keeps its existing
// call-sites unchanged. Other payer packages import scrapeutil directly.

import "insurance-benefit-agent-go/internal/scrapeutil"

var (
	normalizeSpace      = scrapeutil.NormalizeSpace
	normalizeDigits     = scrapeutil.NormalizeDigits
	normalizeDateDigits = scrapeutil.NormalizeDateDigits
	parseMoney          = scrapeutil.ParseMoney
	toSlug              = scrapeutil.ToSlug
	asStringMap         = scrapeutil.AsStringMap
	asSlice             = scrapeutil.AsSlice
	anyStr              = scrapeutil.AnyStr
)
