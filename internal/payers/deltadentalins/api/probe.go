package api

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"

	"insurance-benefit-agent-go/internal/models"
	ddbrowser "insurance-benefit-agent-go/internal/payers/deltadentalins/browser"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/proto"
)

const (
	apiBaseURL             = "https://www.deltadentalins.com/provider-tools/v2/api"
	endpointPatientSearch  = "/patient-mgnt/patient-search"
	endpointBenefitsPkg    = "/benefits/benefits-package"
	endpointTreatmentHist  = "/treatment-history"
	endpointAdditionalBen  = "/benefits/additional-benefits"
	endpointMaxDeductibles = "/benefits/maximums-deductibles"

	// discoveryDummyMemberID and discoveryDummyDOB are synthetic values used
	// solely to trigger the portal's patient-search fetch so we can capture
	// mtvPlocId from the outbound request. No real PHI is used.
	discoveryDummyMemberID = "120000000099"
	discoveryDummyDOB      = "01/01/1990"
)

type BrowserProbe struct {
	session  *ddbrowser.Session
	username string
}

// PracticeContext holds provider/location identifiers required by the API.
// mtvPlocId is the provider location ID shown as "807840681002" in DevTools —
// it must be discovered from the portal or stored in credentials.
type PracticeContext struct {
	DateOfService string `json:"dateOfService"`
	MtvPlocID     string `json:"mtvPlocId"`
	ProviderName  string `json:"providerName"`
}

// PatientSearchRequest is the body sent to /patient-mgnt/patient-search.
type PatientSearchRequest struct {
	MtvPlocID         string `json:"mtvPlocId"`
	DateOfBirth       string `json:"dateOfBirth"`
	IsDependentSearch bool   `json:"isDependentSearch"`
	ContractType      string `json:"contractType"`
	MemberID          string `json:"memberId"`
}

// PatientSearchResult mirrors the response from /patient-mgnt/patient-search.
type PatientSearchResult struct {
	FirstName              string        `json:"firstName"`
	LastName               string        `json:"lastName"`
	PersonID               string        `json:"personId"`
	DateOfBirth            string        `json:"dateOfBirth"`
	MultipleContractsFound bool          `json:"multipleContractsFound"`
	E1                     string        `json:"e1"`
	Card                   PatientCard   `json:"card"`
	Modal                  *PatientModal `json:"modal,omitempty"`
	HCR                    bool          `json:"hcr"`
	Suppressed             bool          `json:"suppressed"`
}

// PatientCard holds the insurance card summary returned in the search response.
type PatientCard struct {
	GroupNumber    string `json:"groupNumber"`
	SubscriberType string `json:"subscriberType"`
	MemberID       string `json:"memberId"`
	MemberCode     string `json:"memberCode"`
}

// PatientModal holds the multi-coverage list returned when a patient has more
// than one active plan. Each coverage entry has its own memberId.
type PatientModal struct {
	Coverages []PatientCoverage `json:"coverages,omitempty"`
}

type PatientCoverage struct {
	GroupNumber         string            `json:"groupNumber"`
	SubscriberType      string            `json:"subscriberType"`
	MemberID            string            `json:"memberId"`
	Plan                string            `json:"plan,omitempty"`
	DivisionName        string            `json:"divisionName,omitempty"`
	MemberAccountStatus string            `json:"memberAccountStatus,omitempty"`
	IsPrimaryPlan       bool              `json:"isPrimaryPlan"`
	EligibilitySpans    []EligibilitySpan `json:"eligibilitySpans,omitempty"`
	E1                  string            `json:"e1,omitempty"`
}

type EligibilitySpan struct {
	StartDate             string `json:"startDate"`
	EndDate               string `json:"endDate"`
	OriginalEffectiveDate string `json:"originalEffectiveDate,omitempty"`
}

// BenefitsPackageResponse mirrors the response from GET /benefits/benefits-package.
type BenefitsPackageResponse struct {
	Member           any                 `json:"member,omitempty"`
	BenefitPackageID string              `json:"benefitPackageId,omitempty"`
	NetworksAllowed  []any               `json:"networksAllowed,omitempty"`
	Treatment        []BenefitsTreatment `json:"treatment,omitempty"`
}

type BenefitsTreatment struct {
	TreatmentCode                string                   `json:"treatmentCode,omitempty"`
	TreatmentDescription         string                   `json:"treatmentDescription,omitempty"`
	TreatmentBusinessDescription string                   `json:"treatmentBusinessDescription,omitempty"`
	SummaryValues                []BenefitsSummaryValue   `json:"summaryValues,omitempty"`
	ProcedureClass               []BenefitsProcedureClass `json:"procedureClass,omitempty"`
}

// BenefitsSummaryValue holds the category-level coverage summary for one network tier.
// NetworkCode values: "##PPO" = PPO, "##PMR" = Premier, "##NP" = Non-Participating (out-of-network).
// The index of each SummaryValue aligns with the corresponding procedure.network[] entry.
type BenefitsSummaryValue struct {
	AmountType      string  `json:"amountType"`
	MaximumCoverage float64 `json:"maximumCoverage"`
	MinimumCoverage float64 `json:"minimumCoverage"`
	NetworkCode     string  `json:"networkCode"`
}

type BenefitsProcedureClass struct {
	Procedure []BenefitsProcedure `json:"procedure,omitempty"`
}

type BenefitsProcedure struct {
	Code                     string            `json:"code,omitempty"`
	Description              string            `json:"description,omitempty"`
	CrossCheckProcedureCodes string            `json:"crossCheckProcedureCodes,omitempty"`
	PreApprovalRequired      bool              `json:"preApprovalRequired"`
	DefaultNetwork           string            `json:"defaultNetwork,omitempty"`
	Network                  []BenefitsNetwork `json:"network,omitempty"`
}

type BenefitsNetwork struct {
	Code           string                   `json:"code,omitempty"`
	CoverageDetail []BenefitsCoverageDetail `json:"coverageDetail,omitempty"`
	Limitation     []BenefitsLimitation     `json:"limitation,omitempty"`
}

type BenefitsCoverageDetail struct {
	BenefitCoverageLevel      string `json:"benefitCoverageLevel,omitempty"`
	CopayAmount               string `json:"copayAmount,omitempty"`
	AmountType                string `json:"amountType,omitempty"`
	DeductibleExempted        bool   `json:"deductibleExempted"`
	MaximumExempted           bool   `json:"maximumExempted"`
	OutOfPocketMaximumApplies bool   `json:"outOfPocketMaximumApplies"`
}

// BenefitsLimitation holds one frequency/quantity restriction on a procedure.
type BenefitsLimitation struct {
	NetworksApplicable       string `json:"networksApplicable,omitempty"`
	BenefitQuantity          int    `json:"benefitQuantity"`
	BenefitCounterIdentifier string `json:"benefitCounterIdentifier,omitempty"`
	FrequencyLimitationText  string `json:"frequencyLimitationText,omitempty"`
	PeriodTypeCode           string `json:"periodTypeCode,omitempty"`
	IntervalUnitCode         string `json:"intervalUnitCode,omitempty"`
	IntervalNumber           int    `json:"intervalNumber"`
}

// MaximumsDeductiblesResponse mirrors the response from GET /benefits/maximums-deductibles.
type MaximumsDeductiblesResponse struct {
	MemberInfo      *MaxMemberInfo          `json:"memberInfo,omitempty"`
	MaximumsInfo    []MaximumsInfoRecord    `json:"maximumsInfo,omitempty"`
	DeductiblesInfo []DeductiblesInfoRecord `json:"deductiblesInfo,omitempty"`
}

// MaximumsInfoRecord is one entry in the maximumsInfo array.
type MaximumsInfoRecord struct {
	AmountInfo      MaxAmountInfo `json:"amountInfo"`
	MaximumDetails  MaxDetails    `json:"maximumDetails"`
	ServicesAllowed []MaxService  `json:"servicesAllowed,omitempty"`
}

type MaxAmountInfo struct {
	RemainingAmount float64 `json:"remainingAmount"`
	TotalAmount     float64 `json:"totalAmount"`
	TotalUsedAmount float64 `json:"totalUsedAmount"`
}

type MaxDetails struct {
	Type                             string `json:"type"`
	CalendarOrContractClassification string `json:"calendarOrContractClassification"`
	AccumPeriodStartDate             string `json:"accumPeriodStartDate"`
	AccumPeriodEndDate               string `json:"accumPeriodEndDate"`
	MaximumCounterID                 string `json:"maximumCounterId,omitempty"`
}

// DeductiblesInfoRecord is one entry in the deductiblesInfo array.
type DeductiblesInfoRecord struct {
	AmountInfo        MaxAmountInfo `json:"amountInfo"`
	DeductibleDetails MaxDetails    `json:"deductibleDetails"`
	ServicesAllowed   []MaxService  `json:"servicesAllowed,omitempty"`
}

type MaxService struct {
	NetworksApplicable       string        `json:"networksApplicable"`
	TreatmentTypeCode        string        `json:"treatmentTypeCode"`
	TreatmentTypeDescription string        `json:"treatmentTypeDescription"`
	ProcedureCodesAllowed    []MaxProcCode `json:"procedureCodesAllowed,omitempty"`
}

// MaxProcCode is one entry in servicesAllowed[].procedureCodesAllowed[].
// It holds the CDT code that counts against the parent accumulator/deductible.
type MaxProcCode struct {
	Code        string `json:"code"`
	Description string `json:"description"`
}

type MaxMemberInfo struct {
	EnrolleeID       string `json:"enrolleeId,omitempty"`
	ContractID       string `json:"contractId,omitempty"`
	PersonID         string `json:"personId,omitempty"`
	BenefitPackageID string `json:"benefitPackageId,omitempty"`
	MemberName       string `json:"memberName,omitempty"`
	BirthDate        string `json:"birthDate,omitempty"`
	Age              string `json:"age,omitempty"`
	GroupNumber      string `json:"groupNumber,omitempty"`
	DivisionNumber   string `json:"divisionNumber,omitempty"`
	DefaultNetwork   string `json:"defaultNetwork,omitempty"`
}

// AdditionalBenefitsResponse mirrors the response from GET /benefits/additional-benefits.
type AdditionalBenefitsResponse struct {
	GroupNumber        string              `json:"groupNumber,omitempty"`
	DivisionNumber     string              `json:"divisionNumber,omitempty"`
	BenefitPackageID   string              `json:"benefitPackageId,omitempty"`
	AdditionalBenefits []AdditionalBenefit `json:"additionalBenefits,omitempty"`
}

type AdditionalBenefit struct {
	Header string `json:"header"`
	Text   string `json:"text"`
}

// TreatmentHistoryResponse mirrors the response from GET /treatment-history.
type TreatmentHistoryResponse struct {
	Procedures []TreatmentProcedure `json:"procedures,omitempty"`
}

type TreatmentProcedure struct {
	Code            string `json:"code"`
	Description     string `json:"description"`
	LastServiceDate string `json:"lastServiceDate"`
}

// PatientAPIBundle is the raw API response bag written to disk per appointment.
type PatientAPIBundle struct {
	PayerURL            string                       `json:"payerUrl"`
	Appointment         models.Appointment           `json:"appointment"`
	Practice            PracticeContext              `json:"practice"`
	SearchResult        PatientSearchResult          `json:"searchResult"`
	BenefitsPackages    []*BenefitsPackageResponse   `json:"benefitsPackages,omitempty"`
	TreatmentHistory    *TreatmentHistoryResponse    `json:"treatmentHistory,omitempty"`
	AdditionalBenefits  *AdditionalBenefitsResponse  `json:"additionalBenefits,omitempty"`
	MaximumsDeductibles *MaximumsDeductiblesResponse `json:"maximumsDeductibles,omitempty"`
	FetchedAt           string                       `json:"fetchedAt"`
}

func NewBrowserProbe(session *ddbrowser.Session) *BrowserProbe {
	return &BrowserProbe{session: session}
}

// DiscoverPracticeContext reads the provider location ID from the portal.
// If mtvPlocID is empty (not yet stored in the credential), it navigates to
// the patient-search page, performs one UI search using a synthetic dummy
// member ID (no real PHI), and captures mtvPlocId from the portal's outbound fetch request.
func (p *BrowserProbe) DiscoverPracticeContext(dateOfService, mtvPlocID, username string) (*PracticeContext, error) {
	p.username = username

	if mtvPlocID == "" {
		log.Printf("[DeltaDental] mtvPlocId not in credential — discovering from portal UI")
		var err error
		mtvPlocID, err = p.discoverMtvPlocIDFromPortal()
		if err != nil {
			return nil, fmt.Errorf("discover mtvPlocId: %w", err)
		}
		log.Printf("[DeltaDental] discovered mtvPlocId=%s", mtvPlocID)
	}

	ctx := &PracticeContext{
		DateOfService: dateOfService,
		MtvPlocID:     mtvPlocID,
	}
	log.Printf("[DeltaDental] practice context: mtvPlocId=%q", mtvPlocID)
	return ctx, nil
}

// discoverMtvPlocIDFromPortal navigates to the patient-search page, injects a
// fetch interceptor, performs one UI-driven patient search, and returns the
// mtvPlocId captured from the portal's own outbound request body.
// A synthetic dummy member ID/DOB is used — no real appointment PHI is sent.
func (p *BrowserProbe) discoverMtvPlocIDFromPortal() (string, error) {
	page := p.session.Page()

	// Navigate first — injection before Navigate is wiped when the page reloads.
	const patientSearchURL = "https://www.deltadentalins.com/provider-tools/v2/patient-search"
	if err := page.Navigate(patientSearchURL); err != nil {
		return "", fmt.Errorf("navigate to patient-search: %w", err)
	}
	_ = page.Timeout(8 * time.Second).WaitLoad()

	// Inject fetch hook AFTER load so the page context is live.
	// The portal includes mtvPlocId in every patient-search request body —
	// we capture it from there regardless of whether the patient is found.
	_, err := page.Eval(`() => {
		window.__ddMtvPlocId = "";
		const _orig = window.fetch;
		window.fetch = function(url, opts) {
			if (typeof url === "string" && url.includes("patient-search") && opts && opts.body) {
				try {
					const b = JSON.parse(opts.body);
					if (b.mtvPlocId) window.__ddMtvPlocId = b.mtvPlocId;
				} catch(_) {}
			}
			return _orig.apply(this, arguments);
		};
	}`)
	if err != nil {
		return "", fmt.Errorf("inject fetch hook: %w", err)
	}

	// Click the "Search by member ID" tab to reveal the member ID / DOB form.
	// The portal may have multiple tabs (e.g. "Search by name") so we match by text.
	deadline := time.Now().Add(10 * time.Second)
	var memberTabEl *rod.Element
	for time.Now().Before(deadline) {
		el, err := page.ElementR(`button[role="tab"]`, "Search by member ID")
		if err == nil {
			if v, _ := el.Visible(); v {
				memberTabEl = el
				break
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	if memberTabEl == nil {
		return "", fmt.Errorf("'Search by member ID' tab not found on patient-search page")
	}
	_ = memberTabEl.Click(proto.InputMouseButtonLeft, 1)
	time.Sleep(500 * time.Millisecond)

	// Fill in member ID — id="memberId" per portal HTML.
	memberIDEl := waitForOneOf(page, []string{`input#memberId`}, 10*time.Second)
	if memberIDEl == nil {
		return "", fmt.Errorf("member ID input not found on patient-search page")
	}
	_ = memberIDEl.Input(discoveryDummyMemberID)

	// Fill in date of birth — id="dob", format MM/DD/YYYY per portal HTML.
	if dobEl := waitForOneOf(page, []string{`input#dob`}, 5*time.Second); dobEl != nil {
		_ = dobEl.Input(discoveryDummyDOB)
	}

	// Click the Search button — data-testid="searchButton" per portal HTML.
	if searchEl := waitForOneOf(page, []string{`button[data-testid="searchButton"]`}, 5*time.Second); searchEl != nil {
		_ = searchEl.Click(proto.InputMouseButtonLeft, 1)
	}

	// Poll for mtvPlocId captured from the fetch hook.
	pollDeadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(pollDeadline) {
		res, err := page.Eval(`() => window.__ddMtvPlocId`)
		if err == nil {
			if id := strings.TrimSpace(res.Value.Str()); id != "" {
				return id, nil
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	return "", fmt.Errorf("timed out waiting for mtvPlocId from portal fetch hook")
}

// waitForOneOf returns the first visible element matching any of the selectors.
func waitForOneOf(page *rod.Page, selectors []string, timeout time.Duration) *rod.Element {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		for _, sel := range selectors {
			el, err := page.Element(sel)
			if err != nil {
				continue
			}
			if v, _ := el.Visible(); v {
				return el
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	return nil
}

// discoverMtvPlocID tries several known profile endpoints to extract the provider
// location ID, then falls back to JS window state inspection.
func (p *BrowserProbe) discoverMtvPlocID() string {
	if p == nil || p.session == nil || p.session.Page() == nil {
		return ""
	}

	// Candidate profile/context endpoints — log responses to find the real one.
	candidates := []string{
		"/provider-mgnt/provider-profile",
		"/provider-mgnt/provider-context",
		"/provider-mgnt/context",
		"/user/profile",
		"/user/context",
		"/profile",
		"/context",
	}
	for _, path := range candidates {
		body, status, err := getThroughPage(p.session.Page(), apiBaseURL+path, map[string]string{
			"accept": "application/json",
		})
		if err != nil {
			log.Printf("[DeltaDental] GET %s error: %v", path, err)
			continue
		}
		_ = status
		if id := extractJSONField(body, "mtvPlocId", "plocId", "providerLocationId", "locationId"); id != "" {
			return id
		}
	}
	return ""
}

// extractJSONField scans raw JSON text for the first matching key and returns its string value.
func extractJSONField(body string, keys ...string) string {
	for _, key := range keys {
		_, after, found := strings.Cut(body, `"`+key+`"`)
		if !found {
			continue
		}
		after = strings.TrimSpace(after)
		if !strings.HasPrefix(after, ":") {
			continue
		}
		after = strings.TrimSpace(after[1:])
		if strings.HasPrefix(after, `"`) {
			val, _, ok := strings.Cut(after[1:], `"`)
			if ok {
				return val
			}
		}
	}
	return ""
}

// getThroughPage executes a GET fetch inside the browser page (cookies included).
func getThroughPage(page *rod.Page, rawURL string, headers map[string]string) (string, int, error) {
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
		.then(async res => JSON.stringify({ status: res.status, text: await res.text() }))`,
		string(quotedURL), string(quotedHeaders))
	res, err := page.Eval(js)
	if err != nil {
		return "", 0, err
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

// SearchAndFetchPatient searches for a member by subscriber ID and date of birth,
// then fetches the benefits package using the enrollee ID from the search result.
func (p *BrowserProbe) SearchAndFetchPatient(ctx PracticeContext, appointment models.Appointment) (*PatientAPIBundle, error) {
	bundle := &PatientAPIBundle{
		PayerURL:    "DeltaDentalIns.com",
		Appointment: appointment,
		Practice:    ctx,
		FetchedAt:   time.Now().UTC().Format(time.RFC3339),
	}

	reqBody := PatientSearchRequest{
		MtvPlocID:         ctx.MtvPlocID,
		DateOfBirth:       toMMDDYYYY(appointment.DOB),
		IsDependentSearch: false,
		ContractType:      "FFS",
		MemberID:          strings.TrimSpace(appointment.SubscriberID),
	}

	var result PatientSearchResult
	if err := p.postJSON(endpointPatientSearch, reqBody, &result); err != nil {
		return nil, fmt.Errorf("patient-search: %w", err)
	}
	bundle.SearchResult = result
	log.Printf("[DeltaDental] patient-search patNum=%s personId=%s name=%s %s e1=%s",
		appointment.PatNum, result.PersonID, result.FirstName, result.LastName, result.E1)

	if result.E1 != "" {
		p.fetchAllForEnrollee(bundle, appointment, result.E1)
	} else if result.Modal != nil && len(result.Modal.Coverages) > 0 {
		log.Printf("[DeltaDental] multiple coverages patNum=%s count=%d",
			appointment.PatNum, len(result.Modal.Coverages))
		winner := SelectWinningCoverage(result.Modal.Coverages, ctx.DateOfService)
		if winner != nil {
			log.Printf("[DeltaDental] selected coverage patNum=%s plan=%s e1=%s isPrimary=%v",
				appointment.PatNum, winner.Plan, winner.E1, winner.IsPrimaryPlan)
			p.fetchAllForEnrollee(bundle, appointment, winner.E1)
		} else {
			log.Printf("[DeltaDental] no active coverage on DOS patNum=%s dos=%s",
				appointment.PatNum, ctx.DateOfService)
		}
	} else {
		log.Printf("[DeltaDental] skipping benefits-package patNum=%s: e1 is empty", appointment.PatNum)
	}

	return bundle, nil
}

// CoverageActiveOnDate returns true if the appointment date falls within any
// eligibility span of the coverage. Falls back to memberAccountStatus="Active"
// if no spans are present.
func CoverageActiveOnDate(cov PatientCoverage, dateOfService string) bool {
	dos, err := time.Parse("2006-01-02", dateOfService)
	if err != nil {
		return strings.EqualFold(cov.MemberAccountStatus, "Active")
	}
	if len(cov.EligibilitySpans) == 0 {
		return strings.EqualFold(cov.MemberAccountStatus, "Active")
	}
	for _, span := range cov.EligibilitySpans {
		start, errS := time.Parse("01/02/2006", span.StartDate)
		end, errE := time.Parse("01/02/2006", span.EndDate)
		if errS != nil || errE != nil {
			continue
		}
		if !dos.Before(start) && !dos.After(end) {
			return true
		}
	}
	return false
}

// SelectWinningCoverage picks the single best coverage to use for a given date of service.
// Rules (in priority order):
//  1. Only one active coverage on DOS → use it.
//  2. Multiple active → prefer isPrimaryPlan=true.
//  3. Multiple active, none primary → latest eligibility span start date wins.
func SelectWinningCoverage(coverages []PatientCoverage, dateOfService string) *PatientCoverage {
	var active []PatientCoverage
	for _, cov := range coverages {
		if cov.E1 != "" && CoverageActiveOnDate(cov, dateOfService) {
			active = append(active, cov)
		}
	}
	if len(active) == 0 {
		return nil
	}
	if len(active) == 1 {
		return &active[0]
	}
	// Rule 2: primary plan wins.
	for i := range active {
		if active[i].IsPrimaryPlan {
			return &active[i]
		}
	}
	// Rule 3: latest span start date wins.
	winner := 0
	winnerStart := latestSpanStart(active[0].EligibilitySpans)
	for i := 1; i < len(active); i++ {
		if s := latestSpanStart(active[i].EligibilitySpans); s.After(winnerStart) {
			winner = i
			winnerStart = s
		}
	}
	return &active[winner]
}

func latestSpanStart(spans []EligibilitySpan) time.Time {
	var latest time.Time
	for _, span := range spans {
		if t, err := time.Parse("01/02/2006", span.StartDate); err == nil && t.After(latest) {
			latest = t
		}
	}
	return latest
}

// fetchAllForEnrollee calls every per-enrollee endpoint using the same e1.
// Add new endpoints here as they are discovered.
func (p *BrowserProbe) fetchAllForEnrollee(bundle *PatientAPIBundle, appointment models.Appointment, e1 string) {
	h := map[string]string{"enrolleeid": e1}

	var pkg BenefitsPackageResponse
	if err := p.getJSON(endpointBenefitsPkg, h, &pkg); err != nil {
		log.Printf("[DeltaDental] benefits-package patNum=%s e1=%s: %v", appointment.PatNum, e1, err)
	} else {
		bundle.BenefitsPackages = append(bundle.BenefitsPackages, &pkg)
		log.Printf("[DeltaDental] benefits-package patNum=%s pkgId=%s treatments=%d",
			appointment.PatNum, pkg.BenefitPackageID, len(pkg.Treatment))
	}

	var th TreatmentHistoryResponse
	if err := p.getJSON(endpointTreatmentHist, h, &th); err != nil {
		log.Printf("[DeltaDental] treatment-history patNum=%s e1=%s: %v", appointment.PatNum, e1, err)
	} else if bundle.TreatmentHistory == nil {
		// First active-on-DOS plan wins — matches BenefitsPackages[0] and applyActiveCoverage order.
		bundle.TreatmentHistory = &th
		log.Printf("[DeltaDental] treatment-history patNum=%s procedures=%d",
			appointment.PatNum, len(th.Procedures))
	}

	var ab AdditionalBenefitsResponse
	if err := p.getJSON(endpointAdditionalBen, h, &ab); err != nil {
		log.Printf("[DeltaDental] additional-benefits patNum=%s e1=%s: %v", appointment.PatNum, e1, err)
	} else if bundle.AdditionalBenefits == nil {
		bundle.AdditionalBenefits = &ab
		log.Printf("[DeltaDental] additional-benefits patNum=%s items=%d",
			appointment.PatNum, len(ab.AdditionalBenefits))
	}

	var md MaximumsDeductiblesResponse
	if err := p.getJSON(endpointMaxDeductibles, h, &md); err != nil {
		log.Printf("[DeltaDental] maximums-deductibles patNum=%s e1=%s: %v", appointment.PatNum, e1, err)
	} else if bundle.MaximumsDeductibles == nil {
		bundle.MaximumsDeductibles = &md
		log.Printf("[DeltaDental] maximums-deductibles patNum=%s maximums=%d",
			appointment.PatNum, len(md.MaximumsInfo))
	}
}

func (p *BrowserProbe) postJSON(endpoint string, body any, out any) error {
	if p == nil || p.session == nil || p.session.Page() == nil {
		return fmt.Errorf("browser probe page is not initialized")
	}

	rawURL := apiBaseURL + endpoint

	bodyBytes, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("marshal request body: %w", err)
	}

	headers := map[string]string{
		"accept":       "application/json",
		"content-type": "application/json",
	}
	if p.username != "" {
		headers["pt-userid"] = p.username
	}
	headers["x-b3-traceid"] = newTraceID()
	headers["x-b3-spanid"] = newSpanID()

	respBody, status, err := postThroughPage(p.session.Page(), rawURL, string(bodyBytes), headers)
	if err != nil {
		return err
	}
	if status < 200 || status >= 300 {
		return fmt.Errorf("POST %s returned status=%d body=%s", rawURL, status, truncate(respBody, 240))
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal([]byte(respBody), out); err != nil {
		return fmt.Errorf("decode %s: %w (body=%s)", rawURL, err, truncate(respBody, 240))
	}
	return nil
}

func (p *BrowserProbe) getJSON(endpoint string, extraHeaders map[string]string, out any) error {
	if p == nil || p.session == nil || p.session.Page() == nil {
		return fmt.Errorf("browser probe page is not initialized")
	}

	rawURL := apiBaseURL + endpoint

	headers := map[string]string{
		"accept": "application/json",
	}
	if p.username != "" {
		headers["pt-userid"] = p.username
	}
	headers["x-b3-traceid"] = newTraceID()
	headers["x-b3-spanid"] = newSpanID()
	for k, v := range extraHeaders {
		headers[k] = v
	}

	respBody, status, err := getThroughPage(p.session.Page(), rawURL, headers)
	if err != nil {
		return err
	}
	if status < 200 || status >= 300 {
		return fmt.Errorf("GET %s returned status=%d body=%s", rawURL, status, truncate(respBody, 240))
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal([]byte(respBody), out); err != nil {
		return fmt.Errorf("decode %s: %w (body=%s)", rawURL, err, truncate(respBody, 240))
	}
	return nil
}

// postThroughPage executes a fetch POST inside the browser page so that session
// cookies are automatically included in the request.
func postThroughPage(page *rod.Page, rawURL, body string, headers map[string]string) (string, int, error) {
	if page == nil {
		return "", 0, fmt.Errorf("page is nil")
	}
	quotedURL, err := json.Marshal(rawURL)
	if err != nil {
		return "", 0, err
	}
	quotedBody, err := json.Marshal(body)
	if err != nil {
		return "", 0, err
	}
	quotedHeaders, err := json.Marshal(headers)
	if err != nil {
		return "", 0, err
	}

	js := fmt.Sprintf(`() => fetch(%s, {
		method: "POST",
		credentials: "include",
		headers: %s,
		body: %s
	}).then(async res => JSON.stringify({
		status: res.status,
		ok: res.ok,
		text: await res.text()
	}))`, string(quotedURL), string(quotedHeaders), string(quotedBody))

	res, err := page.Eval(js)
	if err != nil {
		return "", 0, fmt.Errorf("POST through page %s: %w", rawURL, err)
	}
	var payload struct {
		Status int    `json:"status"`
		OK     bool   `json:"ok"`
		Text   string `json:"text"`
	}
	if err := json.Unmarshal([]byte(res.Value.Str()), &payload); err != nil {
		return "", 0, fmt.Errorf("decode POST payload for %s: %w", rawURL, err)
	}
	return payload.Text, payload.Status, nil
}

// toMMDDYYYY converts common date formats to MM/DD/YYYY as the API expects.
func toMMDDYYYY(value string) string {
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
		if t, err := time.Parse(layout, v); err == nil {
			return t.Format("01/02/2006")
		}
	}
	return v
}

func newTraceID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%016x%016x", time.Now().UnixNano(), time.Now().UnixNano())
	}
	return fmt.Sprintf("%016x%016x", b[:8], b[8:])
}

func newSpanID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%016x", time.Now().UnixNano())
	}
	return fmt.Sprintf("%016x", b)
}

func truncate(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	return value[:limit]
}
