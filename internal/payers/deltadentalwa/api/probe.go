// Package api implements the Delta Dental provider portal API probe.
//
// Portal:  https://provider.deltadental.com/dashboard/
// API base: https://portal.deltadental.com/portal/api/v1/providers/edi/
//
// Flow per patient:
//  1. Check the local subscriberHash cache for this member.
//  2. If cache miss → POST /memberSearch to find the member and get their subscriberHash.
//  3. Save the subscriberHash to the cache (keyed by subscriberID + officeKey).
//  4. GET /benefits?subscriberHash=... to retrieve plan/benefits data.
//  5. Write the full PatientAPIBundle to a probe JSON file on disk.

package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"insurance-benefit-agent-go/internal/models"
	ddbrowser "insurance-benefit-agent-go/internal/payers/deltadentalwa/browser"

	"github.com/go-rod/rod"
)

// ErrSessionExpired is returned when the portal API responds with 401/403.
// The adapter can detect this and trigger a fresh login.
var ErrSessionExpired = errors.New("delta dental session expired")

// ── Portal endpoints ──────────────────────────────────────────────────────────

const (
	memberSearchURL = "https://portal.deltadental.com/portal/api/v1/providers/edi/memberSearch?includeInactiveMembers=false"
	benefitsBaseURL = "https://portal.deltadental.com/portal/api/v1/providers/edi/benefits"
)

// ── Request / Response types ──────────────────────────────────────────────────

// MemberSearchRequest is the body sent to /memberSearch.
type MemberSearchRequest struct {
	MemberID  string `json:"memberId"`
	FirstName string `json:"firstName"`
	LastName  string `json:"lastName"`
	DOB       string `json:"dateOfBirth"` // YYYY-MM-DD
}

// MemberSearchResponse is the full payload returned by /memberSearch.
// All confirmed fields from live network capture.
type MemberSearchResponse struct {
	SubscriberFirstName  string                `json:"subscriberFirstName"`
	SubscriberLastName   string                `json:"subscriberLastName"`
	GroupName            string                `json:"groupName"`
	SubscriberHash       string                `json:"subscriberHash"`
	MemberCompanyName    string                `json:"memberCompanyName"`
	MemberCompanyContext MemberCompanyContext  `json:"memberCompanyContext"`
	SubscriberDateOfBirth string               `json:"subscriberDateOfBirth"`
	ActiveStatus         bool                  `json:"activeStatus"`
	ZipCode              string                `json:"zipCode"`
	Dependents           []DDDependent         `json:"dependents"`
}

// MemberCompanyContext holds portal feature flags for this member's company.
type MemberCompanyContext struct {
	SupportsDependents  bool `json:"supportsDependents"`
	SupportsBenefitsDate bool `json:"supportsBenefitsDate"`
}

// DDDependent is one entry in the dependents array.
type DDDependent struct {
	FirstName      string `json:"firstName"`
	LastName       string `json:"lastName"`
	DateOfBirth    string `json:"dateOfBirth"`
	MemberID       string `json:"memberId"`
	Relationship   string `json:"relationship"`
	ActiveStatus   bool   `json:"activeStatus"`
	SubscriberHash string `json:"subscriberHash"`
}

// BenefitsResponse holds the raw /benefits payload.
// Fields are mapped as a generic map until a full network capture is available.
// To extend: add typed fields here and remove from Raw once confirmed.
type BenefitsResponse struct {
	// Typed fields — add here as /benefits fields are confirmed from DevTools.
	AnnualMax        *float64       `json:"annualMaximum,omitempty"`
	AnnualMaxUsed    *float64       `json:"annualMaximumUsed,omitempty"`
	AnnualMaxRemain  *float64       `json:"annualMaximumRemaining,omitempty"`
	Deductible       *float64       `json:"deductible,omitempty"`
	DeductibleUsed   *float64       `json:"deductibleUsed,omitempty"`
	PreventivePct    *float64       `json:"preventiveCoverage,omitempty"`
	BasicPct         *float64       `json:"basicCoverage,omitempty"`
	MajorPct         *float64       `json:"majorCoverage,omitempty"`
	OrthoAllowed     *bool          `json:"orthodonticsAllowed,omitempty"`
	EffectiveDate    string         `json:"effectiveDate,omitempty"`
	TerminationDate  string         `json:"terminationDate,omitempty"`
	// Raw holds the full response for probe artifacts and future field mapping.
	Raw              map[string]any `json:"raw,omitempty"`
}

// PatientAPIBundle is the full per-appointment probe written to disk.
type PatientAPIBundle struct {
	PayerURL     string                `json:"payerUrl"`
	Appointment  models.Appointment    `json:"appointment"`
	MemberSearch *MemberSearchResponse `json:"memberSearch,omitempty"`
	Benefits     *BenefitsResponse     `json:"benefits,omitempty"`
	FetchedAt    string                `json:"fetchedAt"`
	NotFound     bool                  `json:"notFound,omitempty"`
	Inactive     bool                  `json:"inactive,omitempty"`
}

// ── SubscriberHash cache ──────────────────────────────────────────────────────

// hashCacheEntry is one record in the on-disk hash cache.
type hashCacheEntry struct {
	SubscriberHash string `json:"subscriberHash"`
	CachedAt       string `json:"cachedAt"`
	MemberName     string `json:"memberName"`
	ActiveStatus   bool   `json:"activeStatus"`
}

// HashCache persists subscriberHashes to disk so subsequent runs skip /memberSearch.
// Cache file path: dd-hash-cache-{officeKey}.json
// Key format: "{subscriberID}" (trimmed, lowercase)
type HashCache struct {
	mu       sync.Mutex
	path     string
	entries  map[string]hashCacheEntry
	dirty    bool
}

// LoadHashCache reads the cache file (or starts empty if file doesn't exist).
func LoadHashCache(officeKey string) *HashCache {
	path := fmt.Sprintf("dd-hash-cache-%s.json", sanitize(officeKey))
	c := &HashCache{path: path, entries: make(map[string]hashCacheEntry)}
	data, err := os.ReadFile(path)
	if err != nil {
		return c // file not found is fine
	}
	_ = json.Unmarshal(data, &c.entries)
	log.Printf("[DeltaDental] loaded %d cached subscriber hashes from %s", len(c.entries), path)
	return c
}

// Get returns the cached subscriberHash for a subscriber ID (empty string = miss).
func (c *HashCache) Get(subscriberID string) (string, bool) {
	if c == nil {
		return "", false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.entries[cacheKey(subscriberID)]
	if !ok || entry.SubscriberHash == "" {
		return "", false
	}
	return entry.SubscriberHash, true
}

// Set saves a subscriberHash for a subscriber ID.
func (c *HashCache) Set(subscriberID, hash, memberName string, active bool) {
	if c == nil || hash == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[cacheKey(subscriberID)] = hashCacheEntry{
		SubscriberHash: hash,
		CachedAt:       time.Now().UTC().Format(time.RFC3339),
		MemberName:     memberName,
		ActiveStatus:   active,
	}
	c.dirty = true
}

// Flush writes the cache to disk. Safe to call multiple times.
func (c *HashCache) Flush() {
	if c == nil || !c.dirty {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	data, err := json.MarshalIndent(c.entries, "", "  ")
	if err != nil {
		log.Printf("[DeltaDental] hash cache marshal failed: %v", err)
		return
	}
	if err := os.WriteFile(c.path, data, 0o644); err != nil {
		log.Printf("[DeltaDental] hash cache write failed %s: %v", c.path, err)
		return
	}
	c.dirty = false
	log.Printf("[DeltaDental] hash cache saved %d entries → %s", len(c.entries), c.path)
}

func cacheKey(subscriberID string) string {
	return strings.ToLower(strings.TrimSpace(subscriberID))
}

// ── BrowserProbe ──────────────────────────────────────────────────────────────

// BrowserProbe executes API calls through the authenticated browser session
// so session cookies are automatically included in every request.
type BrowserProbe struct {
	session   *ddbrowser.Session
	hashCache *HashCache
}

// NewBrowserProbe creates a probe. Pass a HashCache so subscriber hashes are
// reused across patients in the same run and persisted across runs.
func NewBrowserProbe(session *ddbrowser.Session, cache *HashCache) *BrowserProbe {
	return &BrowserProbe{session: session, hashCache: cache}
}

// SearchAndFetchPatient is the main per-patient pipeline:
//  1. Check hash cache — skip /memberSearch if already cached.
//  2. POST /memberSearch with memberId + firstName + lastName + dateOfBirth.
//  3. Cache the returned subscriberHash.
//  4. GET /benefits?subscriberHash=... to retrieve plan data.
//
// Returns (bundle, nil) on success.
// Returns (bundle with NotFound=true, nil) when the member is not on the portal.
// Returns (nil, err) on hard failures (network, auth, etc.).
func (p *BrowserProbe) SearchAndFetchPatient(appointment models.Appointment) (*PatientAPIBundle, error) {
	bundle := &PatientAPIBundle{
		PayerURL:    "DeltaDentalIns.com",
		Appointment: appointment,
		FetchedAt:   time.Now().UTC().Format(time.RFC3339),
	}

	subscriberID := strings.TrimSpace(appointment.SubscriberID)

	// ── Step 1: check subscriberHash cache ────────────────────────────────────
	hash, cacheHit := p.hashCache.Get(subscriberID)

	if cacheHit {
		log.Printf("[DeltaDental] hash cache HIT patNum=%s subscriberId=%s", appointment.PatNum, subscriberID)
	} else {
		// ── Step 2: member search ─────────────────────────────────────────────
		log.Printf("[DeltaDental] member search patNum=%s subscriberId=%s firstName=%s lastName=%s dob=%s",
			appointment.PatNum, subscriberID, appointment.FName, appointment.LName, appointment.DOB)

		req := MemberSearchRequest{
			MemberID:  subscriberID,
			FirstName: strings.TrimSpace(appointment.FName),
			LastName:  strings.TrimSpace(appointment.LName),
			DOB:       toYYYYMMDD(appointment.DOB),
		}

		var ms MemberSearchResponse
		if err := p.postJSON(memberSearchURL, req, &ms); err != nil {
			if isNotFound(err) {
				log.Printf("[DeltaDental] member not found patNum=%s subscriberId=%s", appointment.PatNum, subscriberID)
				bundle.NotFound = true
				return bundle, nil
			}
			return nil, fmt.Errorf("member search patNum=%s: %w", appointment.PatNum, err)
		}

		bundle.MemberSearch = &ms
		hash = ms.SubscriberHash

		// ── Step 3: cache the subscriberHash ──────────────────────────────────
		memberName := ms.SubscriberFirstName + " " + ms.SubscriberLastName
		p.hashCache.Set(subscriberID, hash, memberName, ms.ActiveStatus)

		log.Printf("[DeltaDental] member found patNum=%s name=%s %s active=%v company=%q hash_len=%d",
			appointment.PatNum, ms.SubscriberFirstName, ms.SubscriberLastName,
			ms.ActiveStatus, ms.MemberCompanyName, len(hash))

		if !ms.ActiveStatus {
			log.Printf("[DeltaDental] member inactive patNum=%s", appointment.PatNum)
			bundle.Inactive = true
			return bundle, nil
		}
	}

	// ── Step 4: fetch benefits using subscriberHash ───────────────────────────
	if hash == "" {
		log.Printf("[DeltaDental] no subscriberHash available patNum=%s — skipping benefits", appointment.PatNum)
		return bundle, nil
	}

	benURL := benefitsBaseURL + "?subscriberHash=" + hash
	var benResp BenefitsResponse
	if err := p.getJSON(benURL, nil, &benResp); err != nil {
		log.Printf("[DeltaDental] benefits fetch failed patNum=%s: %v", appointment.PatNum, err)
		// Benefits failure is non-fatal — return what we have.
	} else {
		bundle.Benefits = &benResp
		log.Printf("[DeltaDental] benefits fetched patNum=%s annualMax=%v preventivePct=%v",
			appointment.PatNum, benResp.AnnualMax, benResp.PreventivePct)
	}

	return bundle, nil
}

// ── HTTP helpers ──────────────────────────────────────────────────────────────

func (p *BrowserProbe) postJSON(rawURL string, body any, out any) error {
	page := p.session.Page()
	if page == nil {
		return fmt.Errorf("browser page is nil")
	}
	bodyBytes, err := json.Marshal(body)
	if err != nil {
		return err
	}
	headers := map[string]string{
		"accept":       "application/json",
		"content-type": "application/json",
	}
	respBody, status, err := postThroughPage(page, rawURL, string(bodyBytes), headers)
	if err != nil {
		return err
	}
	if status == 404 {
		return fmt.Errorf("404 not found")
	}
	if status == 401 || status == 403 {
		return fmt.Errorf("%w (status=%d)", ErrSessionExpired, status)
	}
	if status < 200 || status >= 300 {
		return fmt.Errorf("POST %s status=%d body=%s", rawURL, status, truncate(respBody, 300))
	}
	if out != nil {
		// Try typed unmarshal first; also capture raw map.
		if err := json.Unmarshal([]byte(respBody), out); err != nil {
			return fmt.Errorf("decode %s: %w (body=%s)", rawURL, err, truncate(respBody, 300))
		}
		// Also populate Raw if out is a *BenefitsResponse.
		if ben, ok := out.(*BenefitsResponse); ok && ben.Raw == nil {
			raw := make(map[string]any)
			_ = json.Unmarshal([]byte(respBody), &raw)
			ben.Raw = raw
		}
	}
	return nil
}

func (p *BrowserProbe) getJSON(rawURL string, extraHeaders map[string]string, out any) error {
	page := p.session.Page()
	if page == nil {
		return fmt.Errorf("browser page is nil")
	}
	headers := map[string]string{"accept": "application/json"}
	for k, v := range extraHeaders {
		headers[k] = v
	}
	respBody, status, err := getThroughPage(page, rawURL, headers)
	if err != nil {
		return err
	}
	if status == 401 || status == 403 {
		return fmt.Errorf("%w (status=%d)", ErrSessionExpired, status)
	}
	if status < 200 || status >= 300 {
		return fmt.Errorf("GET %s status=%d body=%s", rawURL, status, truncate(respBody, 300))
	}
	if out != nil {
		if err := json.Unmarshal([]byte(respBody), out); err != nil {
			return fmt.Errorf("decode %s: %w (body=%s)", rawURL, err, truncate(respBody, 300))
		}
		if ben, ok := out.(*BenefitsResponse); ok && ben.Raw == nil {
			raw := make(map[string]any)
			_ = json.Unmarshal([]byte(respBody), &raw)
			ben.Raw = raw
		}
	}
	return nil
}

// postThroughPage executes a POST via browser fetch() so session cookies are included.
func postThroughPage(page *rod.Page, rawURL, body string, headers map[string]string) (string, int, error) {
	quotedURL, _ := json.Marshal(rawURL)
	quotedBody, _ := json.Marshal(body)
	quotedHeaders, _ := json.Marshal(headers)

	js := fmt.Sprintf(`() => fetch(%s, {
		method: "POST",
		credentials: "include",
		headers: %s,
		body: %s
	}).then(async res => JSON.stringify({ status: res.status, text: await res.text() }))`,
		string(quotedURL), string(quotedHeaders), string(quotedBody))

	res, err := page.Eval(js)
	if err != nil {
		return "", 0, fmt.Errorf("POST through page %s: %w", rawURL, err)
	}
	var payload struct {
		Status int    `json:"status"`
		Text   string `json:"text"`
	}
	if err := json.Unmarshal([]byte(res.Value.Str()), &payload); err != nil {
		return "", 0, err
	}
	return payload.Text, payload.Status, nil
}

// getThroughPage executes a GET via browser fetch() so session cookies are included.
func getThroughPage(page *rod.Page, rawURL string, headers map[string]string) (string, int, error) {
	quotedURL, _ := json.Marshal(rawURL)
	quotedHeaders, _ := json.Marshal(headers)

	js := fmt.Sprintf(`() => fetch(%s, {
		credentials: "include",
		headers: %s
	}).then(async res => JSON.stringify({ status: res.status, text: await res.text() }))`,
		string(quotedURL), string(quotedHeaders))

	res, err := page.Eval(js)
	if err != nil {
		return "", 0, fmt.Errorf("GET through page %s: %w", rawURL, err)
	}
	var payload struct {
		Status int    `json:"status"`
		Text   string `json:"text"`
	}
	if err := json.Unmarshal([]byte(res.Value.Str()), &payload); err != nil {
		return "", 0, err
	}
	return payload.Text, payload.Status, nil
}

// ── Utilities ─────────────────────────────────────────────────────────────────

func isNotFound(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "404") || strings.Contains(msg, "not found")
}

// toYYYYMMDD converts common date formats to YYYY-MM-DD as the API expects.
func toYYYYMMDD(value string) string {
	v := strings.TrimSpace(value)
	if v == "" {
		return ""
	}
	layouts := []string{
		"2006-01-02",
		"01/02/2006",
		"01-02-2006",
		"1/2/2006",
		"2006/01/02",
	}
	for _, layout := range layouts {
		if t, err := time.Parse(layout, v); err == nil {
			return t.Format("2006-01-02")
		}
	}
	return v
}

func sanitize(value string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(value) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	return strings.Trim(b.String(), "-")
}

func truncate(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	return value[:limit]
}

// ProbeFilePath returns the standard path for a probe file.
func ProbeFilePath(dir string, appointment models.Appointment) string {
	return filepath.Join(dir, fmt.Sprintf("DeltaDentalIns_com_%s_%s_api_probe.json",
		sanitize(appointment.PatNum), sanitize(appointment.AptNum)))
}
