package api

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"log"
	"net/url"
	"path"
	"strings"
	"time"

	"insurance-benefit-agent-go/internal/models"
	dqbrowser "insurance-benefit-agent-go/internal/payers/dentaquest/browser"

	"github.com/go-rod/rod"
)

const apiBaseURL = "https://providers.dentaquest.com"

type BrowserProbe struct {
	session     *dqbrowser.Session
	accessToken string
	taxID       string
	dtpc        string
}

type PracticeContext struct {
	DateOfService     string `json:"dateOfService"`
	RouteID           string `json:"routeId"`
	BusinessID        string `json:"businessId"`
	ServiceLocationID string `json:"serviceLocationId"`
	ServiceLocation   string `json:"serviceLocationName"`
	PractitionerID    string `json:"practitionerId"`
	PractitionerName  string `json:"practitionerName"`
	AccessPointID     string `json:"accessPointId"`
}

type MemberSearchResult struct {
	ID                  string `json:"id"`
	ContactID           string `json:"contactId"`
	FirstName           string `json:"firstName"`
	LastName            string `json:"lastName"`
	DateOfBirth         string `json:"dateOfBirth"`
	MemberID            string `json:"memberId"`
	MemberIDLastFour    string `json:"memberIdLastFour"`
	RouteID             string `json:"routeId"`
	Message             string `json:"message"`
	EligibilityCoverage []struct {
		EffectiveDate    string `json:"effectiveDate"`
		TerminationDate  string `json:"terminationDate"`
		SubGroupGuid     string `json:"subGroupGuid"`
		ProductType      string `json:"productType"`
		ExcludeProductTy bool   `json:"excludeProductType"`
	} `json:"eligibilityCoverage"`
}

type PatientAPIBundle struct {
	PayerURL      string             `json:"payerUrl"`
	Appointment   models.Appointment `json:"appointment"`
	Practice      PracticeContext    `json:"practice"`
	SearchResult  MemberSearchResult `json:"searchResult"`
	ProbeDebug    *ProbeDebugInfo    `json:"probeDebug,omitempty"`
	MemberInfo    any                `json:"memberInfo,omitempty"`
	PlanInfo      any                `json:"planInfo,omitempty"`
	Enrollment    any                `json:"enrollmentHistory,omitempty"`
	Clinical      any                `json:"clinicalHistory,omitempty"`
	Benefit       any                `json:"planBenefitSummary,omitempty"`
	Family        any                `json:"familyInfo,omitempty"`
	Maximum       any                `json:"maximumDeductible,omitempty"`
	COB           any                `json:"coordinationOfBenefits,omitempty"`
	ClaimAuth     any                `json:"claimAuthHistory,omitempty"`
	TreatmentPlan any                `json:"treatmentPlanEstimateHistory,omitempty"`
	FetchedAt     string             `json:"fetchedAt"`
}

type ProbeDebugInfo struct {
	SearchRequest     map[string]string      `json:"searchRequest,omitempty"`
	SearchResultCount int                    `json:"searchResultCount"`
	SearchCandidates  []SearchCandidateBrief `json:"searchCandidates,omitempty"`
	ChosenSearchIndex int                    `json:"chosenSearchIndex"`
	ChosenSearchWhy   string                 `json:"chosenSearchWhy,omitempty"`
	DetailFetchErrors map[string]string      `json:"detailFetchErrors,omitempty"`
}

type SearchCandidateBrief struct {
	ID               string `json:"id"`
	FirstName        string `json:"firstName"`
	LastName         string `json:"lastName"`
	DateOfBirth      string `json:"dateOfBirth"`
	MemberID         string `json:"memberId"`
	MemberIDLastFour string `json:"memberIdLastFour"`
	RouteID          string `json:"routeId"`
	Message          string `json:"message"`
}

type pickListLocationsResponse struct {
	BusinessID       string `json:"businessId"`
	ServiceLocations []struct {
		ServiceLocationID      string `json:"serviceLocationId"`
		ServiceLocationName    string `json:"serviceLocationName"`
		ServiceLocationAddress string `json:"serviceLocationAddress"`
		FacilityID             string `json:"facilityId"`
	} `json:"serviceLocations"`
}

type practitionersResponse struct {
	BusinessID        string `json:"businessId"`
	ServiceLocationID string `json:"serviceLocationId"`
	Practitioners     []struct {
		PractitionerID   string `json:"practitionerId"`
		PractitionerName string `json:"practitionerName"`
		AccessPointID    string `json:"accessPointId"`
		NPI              string `json:"npi"`
	} `json:"practitioners"`
}

type localStorageSnapshot struct {
	OktaTokenStorage string `json:"oktaTokenStorage"`
	UserTINStorage   string `json:"userTINStorage"`
	LocationsStorage string `json:"locationsStorage"`
	Practitioners    string `json:"practitionersStorage"`
	DTPC             string `json:"dtpc"`
}

type locationStorageEnvelope struct {
	Data struct {
		TaxIDNumber      string `json:"taxIdNumber"`
		BusinessID       string `json:"businessId"`
		ServiceLocations []struct {
			ServiceLocationID   string `json:"serviceLocationId"`
			ServiceLocationName string `json:"serviceLocationName"`
		} `json:"serviceLocations"`
	} `json:"data"`
}

type practitionerStorageEnvelope struct {
	Data struct {
		TaxIDNumber       string `json:"taxIdNumber"`
		BusinessID        string `json:"businessId"`
		ServiceLocationID string `json:"serviceLocationId"`
		Practitioners     []struct {
			PractitionerID   string `json:"practitionerId"`
			PractitionerName string `json:"practitionerName"`
			AccessPointID    string `json:"accessPointId"`
			NPI              string `json:"npi"`
		} `json:"practitioners"`
	} `json:"data"`
}

type oktaTokenStorage struct {
	AccessToken struct {
		AccessToken string `json:"accessToken"`
	} `json:"accessToken"`
}

func NewBrowserProbe(session *dqbrowser.Session) *BrowserProbe {
	return &BrowserProbe{session: session}
}

func (p *BrowserProbe) DiscoverPracticeContext(dateOfService, providerName string) (*PracticeContext, error) {
	snapshot, err := p.readLocalStorageSnapshot()
	if err != nil {
		return nil, err
	}
	if err := p.captureAuthContext(snapshot); err != nil {
		return nil, err
	}

	locationsByTIN := map[string]locationStorageEnvelope{}
	if err := json.Unmarshal([]byte(snapshot.LocationsStorage), &locationsByTIN); err != nil {
		return nil, fmt.Errorf("decode picklist-locations-get-storage: %w", err)
	}
	if len(locationsByTIN) == 0 {
		return nil, fmt.Errorf("picklist-locations-get-storage is empty")
	}

	locationEnvelope, ok := locationsByTIN[strings.TrimSpace(p.taxID)]
	if !ok {
		for _, candidate := range locationsByTIN {
			locationEnvelope = candidate
			ok = true
			break
		}
	}
	if !ok || len(locationEnvelope.Data.ServiceLocations) == 0 {
		return nil, fmt.Errorf("no service locations found in localStorage cache")
	}
	location := locationEnvelope.Data.ServiceLocations[0]

	practitionersByLocation := map[string]practitionerStorageEnvelope{}
	if err := json.Unmarshal([]byte(snapshot.Practitioners), &practitionersByLocation); err != nil {
		return nil, fmt.Errorf("decode picklist-locationid-practitioners-get-storage: %w", err)
	}
	practitionerEnvelope, ok := practitionersByLocation[location.ServiceLocationID]
	if !ok {
		return nil, fmt.Errorf("practitioner cache missing serviceLocationId=%s", location.ServiceLocationID)
	}
	if len(practitionerEnvelope.Data.Practitioners) == 0 {
		return nil, fmt.Errorf("no practitioners found in localStorage cache")
	}

	practitioner := practitionerEnvelope.Data.Practitioners[0]
	target := normalizeName(providerName)
	if target != "" {
		for _, candidate := range practitionerEnvelope.Data.Practitioners {
			if strings.Contains(normalizeName(candidate.PractitionerName), target) {
				practitioner = candidate
				break
			}
		}
	}

	return &PracticeContext{
		DateOfService:     dateOfService,
		RouteID:           "GOV",
		BusinessID:        firstNonEmpty(locationEnvelope.Data.BusinessID, practitionerEnvelope.Data.BusinessID),
		ServiceLocationID: location.ServiceLocationID,
		ServiceLocation:   location.ServiceLocationName,
		PractitionerID:    practitioner.PractitionerID,
		PractitionerName:  practitioner.PractitionerName,
		AccessPointID:     practitioner.AccessPointID,
	}, nil
}

func (p *BrowserProbe) SearchAndFetchPatient(ctx PracticeContext, appointment models.Appointment) (*PatientAPIBundle, error) {
	var results []MemberSearchResult
	searchQuery := map[string]string{
		"dateOfService": ctx.DateOfService,
		"memberDOB":     normalizeAPIDate(appointment.DOB),
		"memberId":      strings.TrimSpace(appointment.SubscriberID),
	}
	if err := p.fetchJSON(
		"/api/member-eligibility/api/provider-portal/v1/eligibility/member-search",
		searchQuery,
		&results,
	); err != nil {
		return nil, err
	}
	if len(results) == 0 {
		return nil, fmt.Errorf("member-search returned no results for patNum=%s subscriberId=%s", appointment.PatNum, appointment.SubscriberID)
	}

	result, chosenIndex, chosenWhy := chooseSearchResult(results, appointment)
	bundle := &PatientAPIBundle{
		PayerURL:     "DentaQuest.com",
		Appointment:  appointment,
		Practice:     ctx,
		SearchResult: result,
		ProbeDebug: &ProbeDebugInfo{
			SearchRequest:     cloneStringMap(searchQuery),
			SearchResultCount: len(results),
			SearchCandidates:  summarizeSearchResults(results),
			ChosenSearchIndex: chosenIndex,
			ChosenSearchWhy:   chosenWhy,
		},
		FetchedAt: time.Now().UTC().Format(time.RFC3339),
	}
	log.Printf("[DentaQuest API Probe] member-search patNum=%s aptNum=%s subscriberId=%s dob=%s results=%d chosen=%d reason=%s message=%q",
		appointment.PatNum,
		appointment.AptNum,
		searchQuery["memberId"],
		searchQuery["memberDOB"],
		len(results),
		chosenIndex,
		chosenWhy,
		result.Message,
	)
	if strings.TrimSpace(result.ID) == "" {
		log.Printf("[DentaQuest API Probe] patNum=%s aptNum=%s search returned no member id; skipping detail fetches", appointment.PatNum, appointment.AptNum)
		return bundle, nil
	}

	memberPath := path.Join("/api/member-detail/api/provider-portal/v1/member-detail", result.ID)
	route := firstNonEmpty(result.RouteID, ctx.RouteID)
	dateOfService := ctx.DateOfService

	p.fetchSection(bundle, appointment, "memberInfo", memberPath+"/member-info", map[string]string{
		"serviceDate": dateOfService,
		"routeId":     route,
	}, &bundle.MemberInfo)
	p.fetchSection(bundle, appointment, "planInfo", memberPath+"/plan-info", map[string]string{
		"accessPointGuid": ctx.AccessPointID,
		"routeId":         route,
		"serviceDate":     dateOfService,
	}, &bundle.PlanInfo)
	p.fetchSection(bundle, appointment, "enrollmentHistory", memberPath+"/enrollment-history", map[string]string{
		"routeId":       route,
		"dateOfService": dateOfService,
	}, &bundle.Enrollment)
	p.fetchSection(bundle, appointment, "clinicalHistory", memberPath+"/clinical-history", map[string]string{
		"routeId": route,
	}, &bundle.Clinical)
	p.fetchSection(bundle, appointment, "planBenefitSummary", memberPath+"/plan-benefit-summary", map[string]string{
		"routeId":     route,
		"isInNetwork": "true",
		"serviceDate": dateOfService,
	}, &bundle.Benefit)
	p.fetchSection(bundle, appointment, "familyInfo", memberPath+"/family-info", map[string]string{
		"serviceDate": dateOfService,
		"routeId":     route,
	}, &bundle.Family)
	p.fetchSection(bundle, appointment, "maximumDeductible", memberPath+"/maximum-deductible", map[string]string{
		"routeId":     route,
		"isInNetwork": "true",
		"serviceDate": dateOfService,
	}, &bundle.Maximum)
	p.fetchSection(bundle, appointment, "coordinationOfBenefits", memberPath+"/coordination-of-benefits", map[string]string{
		"routeId": route,
	}, &bundle.COB)
	p.fetchSection(bundle, appointment, "claimAuthHistory", memberPath+"/claim-auth-history", map[string]string{
		"routeId": route,
	}, &bundle.ClaimAuth)
	p.fetchSection(bundle, appointment, "treatmentPlanEstimateHistory", memberPath+"/treatment-plan-estimate-history", map[string]string{
		"routeId": route,
	}, &bundle.TreatmentPlan)

	return bundle, nil
}

func (p *BrowserProbe) fetchSection(bundle *PatientAPIBundle, appointment models.Appointment, sectionName, endpoint string, query map[string]string, out any) {
	if err := p.fetchJSON(endpoint, query, out); err != nil {
		if bundle != nil && bundle.ProbeDebug != nil {
			if bundle.ProbeDebug.DetailFetchErrors == nil {
				bundle.ProbeDebug.DetailFetchErrors = map[string]string{}
			}
			bundle.ProbeDebug.DetailFetchErrors[sectionName] = err.Error()
		}
		log.Printf("[DentaQuest API Probe] patNum=%s aptNum=%s section=%s fetch failed: %v", appointment.PatNum, appointment.AptNum, sectionName, err)
		return
	}
	log.Printf("[DentaQuest API Probe] patNum=%s aptNum=%s section=%s status=ok", appointment.PatNum, appointment.AptNum, sectionName)
}

func (p *BrowserProbe) fetchJSON(endpoint string, query map[string]string, out any) error {
	if p == nil || p.session == nil || p.session.Page() == nil {
		return fmt.Errorf("browser probe page is not initialized")
	}

	rawURL, err := buildURL(endpoint, query)
	if err != nil {
		return err
	}
	if err := p.ensureAuthContext(); err != nil {
		return err
	}
	headers := map[string]string{
		"accept": "application/json, text/plain, */*",
	}
	if strings.TrimSpace(p.accessToken) != "" {
		headers["authorization"] = "Bearer " + strings.TrimSpace(p.accessToken)
	}
	if strings.TrimSpace(p.taxID) != "" {
		headers["x-tax-id-number"] = strings.TrimSpace(p.taxID)
	}
	if strings.TrimSpace(p.dtpc) != "" {
		headers["x-dtpc"] = strings.TrimSpace(p.dtpc)
	}
	headers["x-traceability-id"] = newTraceabilityID()

	body, status, err := fetchThroughPage(p.session.Page(), rawURL, headers)
	if err != nil {
		return err
	}
	if status < 200 || status >= 300 {
		return fmt.Errorf("fetch %s returned status=%d body=%s", rawURL, status, truncate(body, 240))
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal([]byte(body), out); err != nil {
		return fmt.Errorf("decode %s: %w", rawURL, err)
	}
	return nil
}

func fetchThroughPage(page *rod.Page, rawURL string, headers map[string]string) (string, int, error) {
	if page == nil {
		return "", 0, fmt.Errorf("page is nil")
	}

	quotedURL, err := json.Marshal(rawURL)
	if err != nil {
		return "", 0, err
	}
	quotedHeaders, err := json.Marshal(headers)
	if err != nil {
		return "", 0, err
	}
	js := fmt.Sprintf(`() => fetch(%s, { credentials: "include", headers: %s })
		.then(async (res) => JSON.stringify({
			status: res.status,
			ok: res.ok,
			text: await res.text()
		}))`, string(quotedURL), string(quotedHeaders))

	res, err := page.Eval(js)
	if err != nil {
		return "", 0, fmt.Errorf("fetch through page %s: %w", rawURL, err)
	}

	var payload struct {
		Status int    `json:"status"`
		OK     bool   `json:"ok"`
		Text   string `json:"text"`
	}
	if err := json.Unmarshal([]byte(res.Value.Str()), &payload); err != nil {
		return "", 0, fmt.Errorf("decode fetch payload for %s: %w", rawURL, err)
	}
	return payload.Text, payload.Status, nil
}

func chooseSearchResult(results []MemberSearchResult, appointment models.Appointment) (MemberSearchResult, int, string) {
	targetDOB := normalizeAPIDate(appointment.DOB)
	targetID := strings.TrimSpace(appointment.SubscriberID)
	for i, result := range results {
		if targetDOB != "" && result.DateOfBirth != "" && result.DateOfBirth != targetDOB {
			continue
		}
		if targetID != "" && strings.EqualFold(strings.TrimSpace(result.MemberID), targetID) {
			return result, i, "memberId_match"
		}
	}
	return results[0], 0, "fallback_first_result"
}

func summarizeSearchResults(results []MemberSearchResult) []SearchCandidateBrief {
	out := make([]SearchCandidateBrief, 0, len(results))
	for _, result := range results {
		out = append(out, SearchCandidateBrief{
			ID:               result.ID,
			FirstName:        result.FirstName,
			LastName:         result.LastName,
			DateOfBirth:      result.DateOfBirth,
			MemberID:         result.MemberID,
			MemberIDLastFour: result.MemberIDLastFour,
			RouteID:          result.RouteID,
			Message:          result.Message,
		})
	}
	return out
}

func cloneStringMap(src map[string]string) map[string]string {
	if len(src) == 0 {
		return nil
	}
	dst := make(map[string]string, len(src))
	for key, value := range src {
		dst[key] = value
	}
	return dst
}

func buildURL(endpoint string, query map[string]string) (string, error) {
	base, err := url.Parse(apiBaseURL)
	if err != nil {
		return "", err
	}
	base.Path = endpoint
	values := url.Values{}
	for key, value := range query {
		if strings.TrimSpace(value) == "" {
			continue
		}
		values.Set(key, value)
	}
	base.RawQuery = values.Encode()
	return base.String(), nil
}

func normalizeAPIDate(value string) string {
	v := strings.TrimSpace(value)
	if v == "" {
		return ""
	}
	layouts := []string{
		"2006-01-02",
		"01-02-2006",
		"01/02/2006",
		"1/2/2006",
		"2006/01/02",
	}
	for _, layout := range layouts {
		if parsed, err := time.Parse(layout, v); err == nil {
			return parsed.Format("2006-01-02")
		}
	}
	return v
}

func normalizeName(value string) string {
	replacer := strings.NewReplacer(" ", "", "-", "", "'", "")
	return replacer.Replace(strings.ToLower(strings.TrimSpace(value)))
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func truncate(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	return value[:limit]
}

func (p *BrowserProbe) ensureAuthContext() error {
	if strings.TrimSpace(p.accessToken) != "" && strings.TrimSpace(p.taxID) != "" && strings.TrimSpace(p.dtpc) != "" {
		return nil
	}
	snapshot, err := p.readLocalStorageSnapshot()
	if err != nil {
		return err
	}
	return p.captureAuthContext(snapshot)
}

func (p *BrowserProbe) captureAuthContext(snapshot *localStorageSnapshot) error {
	if snapshot == nil {
		return fmt.Errorf("localStorage snapshot is nil")
	}
	if strings.TrimSpace(snapshot.OktaTokenStorage) == "" {
		return fmt.Errorf("okta-token-storage is empty")
	}

	var tokens oktaTokenStorage
	if err := json.Unmarshal([]byte(snapshot.OktaTokenStorage), &tokens); err != nil {
		return fmt.Errorf("decode okta-token-storage: %w", err)
	}
	p.accessToken = strings.TrimSpace(tokens.AccessToken.AccessToken)
	p.taxID = strings.TrimSpace(snapshot.UserTINStorage)
	p.dtpc = strings.TrimSpace(snapshot.DTPC)
	if p.accessToken == "" {
		return fmt.Errorf("access token missing from okta-token-storage")
	}
	if p.taxID == "" {
		return fmt.Errorf("user-tin-storage is empty")
	}
	if p.dtpc == "" {
		return fmt.Errorf("dtPC is empty")
	}
	return nil
}

func (p *BrowserProbe) readLocalStorageSnapshot() (*localStorageSnapshot, error) {
	if p == nil || p.session == nil || p.session.Page() == nil {
		return nil, fmt.Errorf("browser probe page is not initialized")
	}
	res, err := p.session.Page().Eval(`() => JSON.stringify({
		oktaTokenStorage: localStorage.getItem("okta-token-storage") || "",
		userTINStorage: localStorage.getItem("user-tin-storage") || "",
		locationsStorage: localStorage.getItem("picklist-locations-get-storage") || "",
		practitionersStorage: localStorage.getItem("picklist-locationid-practitioners-get-storage") || "",
		dtpc: (document.cookie.match(/(?:^|;\s*)dtPC=([^;]+)/) || [])[1] || ""
	})`)
	if err != nil {
		return nil, fmt.Errorf("read DentaQuest localStorage: %w", err)
	}
	var snapshot localStorageSnapshot
	if err := json.Unmarshal([]byte(res.Value.Str()), &snapshot); err != nil {
		return nil, fmt.Errorf("decode DentaQuest localStorage snapshot: %w", err)
	}
	return &snapshot, nil
}

func newTraceabilityID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("trace-%d", time.Now().UnixNano())
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		uint32(b[0])<<24|uint32(b[1])<<16|uint32(b[2])<<8|uint32(b[3]),
		uint16(b[4])<<8|uint16(b[5]),
		uint16(b[6])<<8|uint16(b[7]),
		uint16(b[8])<<8|uint16(b[9]),
		uint64(b[10])<<40|uint64(b[11])<<32|uint64(b[12])<<24|uint64(b[13])<<16|uint64(b[14])<<8|uint64(b[15]),
	)
}
