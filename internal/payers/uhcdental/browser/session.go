// Package browser handles the UHC Dental Optum SSO login via a headed/headless
// browser. After a successful login the session extracts all provider-level
// cookies and makes them available to the API probe.
package browser

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/proto"

	sharedbrowser "insurance-benefit-agent-go/internal/browser"
	"insurance-benefit-agent-go/internal/payers"
	uhcapi "insurance-benefit-agent-go/internal/payers/uhcdental/api"
)

const loginURL = "https://secure.uhcdental.com"

// Session wraps the shared browser session for UHC Dental login.
type Session struct {
	inner              *sharedbrowser.Session
	sessionReadyLogged bool
}

const searchLandingURL = "https://secure.uhcdental.com/content/dental-benefits-provider/en/secure/search-landing.html"
const dashboardURL = "https://secure.uhcdental.com/content/dental-benefits-provider/en/secure/postloginhomescreen.html"

// Launch creates a clean browser session for UHC Dental using the shared
// launcher path used by the other payers.
func Launch(input payers.SessionInput) (*Session, error) {
	opts := sharedbrowser.LaunchOptions{Headless: input.Headless}
	inner, err := sharedbrowser.Launch(opts)
	if err != nil {
		return nil, fmt.Errorf("launch browser: %w", err)
	}

	s := &Session{inner: inner}
	if err := s.login(input); err != nil {
		s.Close()
		return nil, fmt.Errorf("UHC Dental login: %w", err)
	}
	if err := s.waitForProviderCookies(30 * time.Second); err != nil {
		s.Close()
		return nil, fmt.Errorf("UHC Dental provider cookies: %w", err)
	}
	return s, nil
}

// waitForProviderCookies polls until PROVIDERID and optumId are set by the
// dashboard page JS, or until the timeout expires.
func (s *Session) waitForProviderCookies(timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		res, err := proto.StorageGetCookies{}.Call(s.inner.Browser)
		if err == nil {
			var hasProvider, hasOptum bool
			for _, c := range res.Cookies {
				if c.Name == "PROVIDERID" && c.Value != "" {
					hasProvider = true
				}
				if c.Name == "optumId" && c.Value != "" {
					hasOptum = true
				}
			}
			if hasProvider && hasOptum {
				log.Printf("[UHCDental] provider cookies ready (PROVIDERID + optumId set)")
				return nil
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	return fmt.Errorf("timed out waiting for PROVIDERID/optumId cookies after %s", timeout)
}

// login performs the Optum SSO login flow using the persistent chrome-profile
// which holds device-trust cookies from a prior manual login. This means OTP
// is skipped on subsequent runs. If automated login fails for any reason the
// function falls back to waiting for the user to log in manually.
func (s *Session) login(input payers.SessionInput) error {
	page := s.inner.Page

	log.Printf("[UHCDental] navigating to %s", loginURL)
	if err := page.Navigate(loginURL); err != nil {
		return fmt.Errorf("navigate to login page: %w", err)
	}
	_ = page.Timeout(15 * time.Second).WaitLoad()

	// If already logged in (session cookie still valid), portal loads directly.
	if info, _ := page.Info(); strings.Contains(info.URL, "postloginhomescreen") ||
		strings.Contains(info.URL, "search-landing") {
		log.Printf("[UHCDental] session still active, skipped login")
		return nil
	}

	// Click LOG IN → Optum SSO page.
	if waitVisible(page, `input[value="LOG IN"]`, 10*time.Second) {
		if err := page.MustElement(`input[value="LOG IN"]`).Click(proto.InputMouseButtonLeft, 1); err != nil {
			log.Printf("[UHCDental] click LOG IN failed: %v — falling back to manual login", err)
			return waitForPortal(page, 5*time.Minute)
		}
		_ = page.Timeout(15 * time.Second).WaitLoad()
	}

	// Fill username.
	usernameSel := `#username, [data-cy="data-username-field"]`
	if !waitVisible(page, usernameSel, 20*time.Second) {
		log.Printf("[UHCDental] username field not found — falling back to manual login")
		return waitForPortal(page, 5*time.Minute)
	}
	el, err := page.Element(usernameSel)
	if err != nil {
		return waitForPortal(page, 5*time.Minute)
	}
	_ = el.Click(proto.InputMouseButtonLeft, 1)
	time.Sleep(300 * time.Millisecond)
	if err := el.Input(input.Credential.Username); err != nil {
		log.Printf("[UHCDental] fill username failed: %v — falling back to manual login", err)
		return waitForPortal(page, 5*time.Minute)
	}
	time.Sleep(500 * time.Millisecond)
	if err := page.MustElement(`#btnLogin`).Click(proto.InputMouseButtonLeft, 1); err != nil {
		log.Printf("[UHCDental] click Continue failed: %v — falling back to manual login", err)
		return waitForPortal(page, 5*time.Minute)
	}

	// Fill password.
	if !waitVisible(page, `#login-pwd`, 20*time.Second) {
		log.Printf("[UHCDental] password field not found — falling back to manual login")
		return waitForPortal(page, 5*time.Minute)
	}
	pwd, err := page.Element(`#login-pwd`)
	if err != nil {
		return waitForPortal(page, 5*time.Minute)
	}
	_ = pwd.Click(proto.InputMouseButtonLeft, 1)
	time.Sleep(300 * time.Millisecond)
	if err := pwd.Input(input.Password); err != nil {
		log.Printf("[UHCDental] fill password failed: %v — falling back to manual login", err)
		return waitForPortal(page, 5*time.Minute)
	}
	time.Sleep(500 * time.Millisecond)
	if err := page.MustElement(`#btnLogin`).Click(proto.InputMouseButtonLeft, 1); err != nil {
		log.Printf("[UHCDental] click Submit failed: %v — falling back to manual login", err)
		return waitForPortal(page, 5*time.Minute)
	}

	// Wait for portal — device trust in chrome-profile means no OTP expected.
	if err := handleMFA(page, input); err != nil {
		log.Printf("[UHCDental] MFA automation failed: %v - falling back to manual login", err)
		return waitForPortal(page, 5*time.Minute)
	}

	if err := waitForPortal(page, 60*time.Second); err != nil {
		log.Printf("[UHCDental] portal not reached automatically (%v) — waiting for manual login", err)
		return waitForPortal(page, 5*time.Minute)
	}
	return nil
}

// StartNetworkLogger enables CDP network events and logs every response to
// /apps/dental/* so you can see what the portal's own JS calls (and in what
// order) when you manually search a patient in the browser.
func (s *Session) StartNetworkLogger() {
	// Intentionally quiet during normal probe runs. Re-enable locally when
	// debugging portal XHR sequencing.
}

// WaitForReady pauses on the dashboard so the user can verify browser state
// before API calls proceed. Press ENTER in the terminal to continue.
func waitForPortal(page *rod.Page, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		info, err := page.Info()
		if err == nil {
			u := info.URL
			if strings.Contains(u, "postloginhomescreen") ||
				strings.Contains(u, "search-landing") ||
				strings.Contains(u, "eligibility-summary") {
				log.Printf("[UHCDental] login successful, reached portal (%s)", u)
				return nil
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	info, _ := page.Info()
	return fmt.Errorf("login timeout: portal not reached (url=%s)", info.URL)
}

// sessionCookieNames is the whitelist of cookies the UHC Dental API needs.
// All are forced to path "/" so the cookie jar sends them to /apps/dental/...
// regardless of the path they were set on by the portal.
var sessionCookieNames = map[string]bool{
	"JSESSIONID":   true,
	"affinity":     true,
	"PROVIDERID":   true,
	"PTIN":         true,
	"optumId":      true,
	"optumUUId":    true,
	"user":         true,
	"commonPracId": true,
}

// ExtractSessionCookies reads all browser cookies and returns the provider-level
// session cookies needed by the API probe. Only whitelisted cookies are included
// in Raw to avoid analytics cookies whose values contain characters (e.g. `"`)
// that Go's net/http drops, which would corrupt the Cookie header.
func (s *Session) ExtractSessionCookies() (uhcapi.SessionCookies, error) {
	res, err := proto.StorageGetCookies{}.Call(s.inner.Browser)
	if err != nil {
		return uhcapi.SessionCookies{}, fmt.Errorf("get browser cookies: %w", err)
	}

	sc := uhcapi.SessionCookies{}
	for _, c := range res.Cookies {
		switch c.Name {
		case "JSESSIONID":
			sc.JSESSIONID = c.Value
		case "affinity":
			sc.Affinity = c.Value
		case "PROVIDERID":
			sc.ProviderID = c.Value
		case "PTIN":
			sc.PTIN = c.Value
		case "optumId":
			sc.OptumID = c.Value
		case "optumUUId":
			sc.OptumUUID = c.Value
		}
		if !sessionCookieNames[c.Name] {
			continue
		}
		// Strip characters that Go's net/http rejects in Cookie headers (e.g. `"`).
		val := strings.Map(func(r rune) rune {
			if r == '"' || r < 0x20 || r == 0x7f {
				return -1
			}
			return r
		}, c.Value)
		sc.Raw = append(sc.Raw, &http.Cookie{
			Name:   c.Name,
			Value:  val,
			Domain: c.Domain,
			Path:   "/", // force root path so jar sends to /apps/dental/...
			Secure: c.Secure,
		})
	}

	if !s.sessionReadyLogged {
		log.Printf("[UHCDental] session cookies ready providerID=%q optumId=%q", sc.ProviderID, sc.OptumID)
		s.sessionReadyLogged = true
	}
	if sc.JSESSIONID == "" {
		return sc, fmt.Errorf("no JSESSIONID in browser cookies — login may have failed")
	}
	return sc, nil
}

// navigateToSearchLanding navigates to the search-landing page. It prefers
// a direct navigation to avoid extra SPA churn while processing many patients.
func (s *Session) navigateToSearchLanding() error {
	page := s.inner.Page
	if info, _ := page.Info(); strings.Contains(info.URL, "search-landing") {
		return waitForSearchLandingReady(page, 10*time.Second)
	}
	if err := page.Navigate(searchLandingURL); err != nil {
		return fmt.Errorf("navigate to search-landing: %w", err)
	}
	_ = page.Timeout(15 * time.Second).WaitLoad()
	return waitForSearchLandingReady(page, 15*time.Second)
}

// FetchPlanID navigates the browser to the portal's own eligibility-summary
// page with memberContrivedKey in the URL, so the portal's Angular app
// initialises, calls eligsummary internally, and sets the planId cookie.
// We poll for the cookie rather than calling eligsummary ourselves — every
// direct attempt (fetch or page.Navigate) hits the AEM Publish CDN tier which
// always returns 500 for that endpoint.
func (s *Session) WaitForPlanID() (string, error) {
	page := s.inner.Page
	productIDCh := make(chan string, 1)
	productIDErrCh := make(chan error, 1)

	go page.EachEvent(func(e *proto.NetworkResponseReceived) {
		u := e.Response.URL
		if !strings.Contains(u, "/apps/dental/eligsummary") {
			return
		}

		bodyRes, err := proto.NetworkGetResponseBody{RequestID: e.RequestID}.Call(page)
		if err != nil {
			select {
			case productIDErrCh <- fmt.Errorf("eligsummary body: %w", err):
			default:
			}
			return
		}

		var payload struct {
			Result struct {
				EligibilitySummary struct {
					ProductID string `json:"productId"`
				} `json:"eligibilitySummary"`
			} `json:"result"`
		}
		if err := json.Unmarshal([]byte(bodyRes.Body), &payload); err != nil {
			select {
			case productIDErrCh <- fmt.Errorf("decode eligsummary body: %w", err):
			default:
			}
			return
		}

		productID := strings.TrimSpace(payload.Result.EligibilitySummary.ProductID)
		if productID == "" {
			select {
			case productIDErrCh <- fmt.Errorf("eligsummary productId missing"):
			default:
			}
			return
		}

		select {
		case productIDCh <- productID:
		default:
		}
	})()

	// Set the memberContrivedKey cookie so the Angular app knows which member.
	info, err := page.Info()
	if err != nil {
		return "", fmt.Errorf("eligibility-summary page info: %w", err)
	}
	if info == nil || !strings.Contains(info.URL, "eligibility-summary") {
		return "", fmt.Errorf("not on eligibility-summary page (url=%s)", info.URL)
	}

	// Navigate to the portal's eligibility-summary page with memberContrivedKey
	// and the service date as query params — the Angular SPA reads these and
	// triggers its own eligsummary XHR, which sets the planId cookie.
	// Poll for the planId cookie the Angular app sets after its eligsummary call.
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if el, err := page.Timeout(500 * time.Millisecond).Element(`#product-id`); err == nil {
			if text, textErr := el.Text(); textErr == nil {
				text = strings.TrimSpace(text)
				if text != "" {
					return text, nil
				}
			}
		}

		select {
		case productID := <-productIDCh:
			return productID, nil
		case err := <-productIDErrCh:
			_ = err
		default:
		}

		res, err := proto.StorageGetCookies{}.Call(s.inner.Browser)
		if err == nil {
			for _, c := range res.Cookies {
				if c.Name == "planId" && c.Value != "" {
					return c.Value, nil
				}
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	return "", fmt.Errorf("planId not found after waiting on eligibility-summary page")
}

func shouldLogUHCEndpoint(url string) bool {
	switch {
	case strings.Contains(url, "/apps/dental/benefitsummary"):
		return true
	case strings.Contains(url, "/apps/dental/utilizationHistory"):
		return true
	case strings.Contains(url, "/apps/dental/claimsummary"):
		return true
	default:
		return false
	}
}

func summarizeUHCEndpoint(url string) string {
	switch {
	case strings.Contains(url, "/apps/dental/member"):
		return "/apps/dental/member"
	case strings.Contains(url, "/apps/dental/eligsummary"):
		return "/apps/dental/eligsummary"
	case strings.Contains(url, "/apps/dental/benefitsummary"):
		return "/apps/dental/benefitsummary"
	case strings.Contains(url, "/apps/dental/utilizationHistory"):
		return "/apps/dental/utilizationHistory"
	case strings.Contains(url, "/apps/dental/claimsummary"):
		return "/apps/dental/claimsummary"
	default:
		return url
	}
}

// SearchMemberViaBrowser calls POST /apps/dental/member from within the browser
// so the server's Set-Cookie response headers land in the browser's cookie jar.
// Those member cookies are what allows eligsummary to succeed — calling member
// search via Go's HTTP client stores the cookies only in the Go jar, not the
// browser's, which is why eligsummary was returning AEM 500.
func (s *Session) SearchMemberViaBrowser(subscriberID, dob, serviceDate string) (*uhcapi.MemberInfo, error) {
	page := s.inner.Page

	if err := s.navigateToSearchLanding(); err != nil {
		return nil, fmt.Errorf("navigate to search-landing: %w", err)
	}

	postBody := "applicationId=DBP" +
		"&dateOfBirth=" + dob +
		"&roleId=DBP" +
		"&maximumConsumerRecordCount=50" +
		"&coverageTypeCode=37" +
		"&timelineIndicator=2" +
		"&sourceCodeIndicator=1" +
		"&asOfDate=" + serviceDate +
		"&familyIndicator=I" +
		"&searchId=" + subscriberID

	result, err := page.Eval(`async (url, body) => {
		const r = await fetch(url, {
			method: 'POST',
			headers: {
				'Content-Type': 'application/xml;',
				'Accept': 'application/json, text/plain, */*',
				'csrf-token': 'undefined',
				'Referer': 'https://secure.uhcdental.com/content/dental-benefits-provider/en/secure/search-landing.html'
			},
			body: body
		});
		return { status: r.status, body: await r.text() };
	}`, "https://secure.uhcdental.com/apps/dental/member", postBody)
	if err != nil && strings.Contains(err.Error(), "Inspected target navigated or closed") {
		log.Printf("[UHCDental] member search interrupted by navigation, retrying after search page settles")
		if navErr := waitForSearchLandingReady(page, 15*time.Second); navErr != nil {
			return nil, fmt.Errorf("browser member search eval: %w", err)
		}
		result, err = page.Eval(`async (url, body) => {
			const r = await fetch(url, {
				method: 'POST',
				headers: {
					'Content-Type': 'application/xml;',
					'Accept': 'application/json, text/plain, */*',
					'csrf-token': 'undefined',
					'Referer': 'https://secure.uhcdental.com/content/dental-benefits-provider/en/secure/search-landing.html'
				},
				body: body
			});
			return { status: r.status, body: await r.text() };
		}`, "https://secure.uhcdental.com/apps/dental/member", postBody)
	}
	if err != nil {
		return nil, fmt.Errorf("browser member search eval: %w", err)
	}

	status := int(result.Value.Get("status").Int())
	bodyStr := result.Value.Get("body").Str()
	if status != 200 {
		snippet := bodyStr
		if len(snippet) > 400 {
			snippet = snippet[:400]
		}
		log.Printf("[UHCDental] browser member search body: %s", snippet)
		return nil, fmt.Errorf("browser member search HTTP %d", status)
	}

	mi, err := uhcapi.ParseMemberSearchBodyForDOB([]byte(bodyStr), dob)
	if err != nil {
		return nil, fmt.Errorf("parse browser member search (subscriberID=%s): %w", subscriberID, err)
	}
	return mi, nil
}

// SearchMemberViaGUI drives the visible search form: fill date of service, DOB,
// and subscriber ID, click Search, then wait for the member-search XHR and the
// eligibility-summary page transition.
func (s *Session) SearchMemberViaGUI(subscriberID, dob, serviceDate string) (*uhcapi.MemberInfo, error) {
	page := s.inner.Page
	if err := s.navigateToSearchLanding(); err != nil {
		return nil, fmt.Errorf("navigate to search-landing: %w", err)
	}

	memberStatusCh := make(chan int, 1)
	memberBodyCh := make(chan string, 1)
	waitMember := page.EachEvent(func(e *proto.NetworkResponseReceived) {
		u := e.Response.URL
		if !strings.Contains(u, "/apps/dental/member") {
			return
		}
		bodyRes, err := proto.NetworkGetResponseBody{RequestID: e.RequestID}.Call(page)
		if err != nil {
			return
		}
		select {
		case memberStatusCh <- e.Response.Status:
		default:
		}
		select {
		case memberBodyCh <- bodyRes.Body:
		default:
		}
	})
	go waitMember()

	if _, err := page.Eval(`(subscriberID, dob, serviceDate) => {
		function setValue(id, value) {
			const el = document.getElementById(id);
			if (!el) {
				throw new Error("element not found: " + id);
			}
			el.focus();
			el.value = value;
			el.dispatchEvent(new Event('input', { bubbles: true }));
			el.dispatchEvent(new Event('change', { bubbles: true }));
			el.dispatchEvent(new Event('blur', { bubbles: true }));
		}
		setValue('serviceDate', serviceDate);
		setValue('eligMemDob', dob);
		setValue('eligSubsId', subscriberID);
	}`, subscriberID, dob, serviceDate); err != nil {
		return nil, fmt.Errorf("fill gui search form: %w", err)
	}

	searchBtn, err := page.Timeout(15 * time.Second).Element(`input[type="button"][value="search"]`)
	if err != nil {
		return nil, fmt.Errorf("find gui search button: %w", err)
	}
	if err := searchBtn.Click(proto.InputMouseButtonLeft, 1); err != nil {
		return nil, fmt.Errorf("click gui search button: %w", err)
	}

	deadline := time.Now().Add(25 * time.Second)
	memberStatus := 0
	memberBody := ""
	seenEligSummary := false
	for time.Now().Before(deadline) {
		select {
		case status := <-memberStatusCh:
			memberStatus = status
		default:
		}
		select {
		case body := <-memberBodyCh:
			memberBody = body
		default:
		}
		if info, err := page.Info(); err == nil && info != nil && strings.Contains(info.URL, "eligibility-summary") {
			seenEligSummary = true
		}
		if memberBody != "" && seenEligSummary {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	if memberStatus != 200 {
		snippet := memberBody
		if len(snippet) > 400 {
			snippet = snippet[:400]
		}
		if snippet != "" {
			log.Printf("[UHCDental] gui member search body: %s", snippet)
		}
		return nil, fmt.Errorf("gui member search HTTP %d", memberStatus)
	}
	if memberBody == "" {
		return nil, fmt.Errorf("gui member search response not captured")
	}
	if !seenEligSummary {
		return nil, fmt.Errorf("eligibility-summary page not reached after gui search")
	}

	mi, err := uhcapi.ParseMemberSearchBodyForDOB([]byte(memberBody), dob)
	if err != nil {
		return nil, fmt.Errorf("parse gui member search (subscriberID=%s): %w", subscriberID, err)
	}
	return mi, nil
}

func (s *Session) SearchMemberAndEligSummaryViaBrowser(subscriberID, dob, serviceDate, providerID string) (*uhcapi.MemberInfo, string, error) {
	page := s.inner.Page
	if err := s.navigateToSearchLanding(); err != nil {
		return nil, "", fmt.Errorf("navigate to search-landing: %w", err)
	}

	memberBody := "applicationId=DBP" +
		"&dateOfBirth=" + dob +
		"&roleId=DBP" +
		"&maximumConsumerRecordCount=50" +
		"&coverageTypeCode=37" +
		"&timelineIndicator=2" +
		"&sourceCodeIndicator=1" +
		"&asOfDate=" + serviceDate +
		"&familyIndicator=I" +
		"&searchId=" + subscriberID

	runFlow := func() (*proto.RuntimeRemoteObject, error) {
		return page.Eval(`async (memberBody, providerID) => {
			const memberResp = await fetch("https://secure.uhcdental.com/apps/dental/member", {
				method: 'POST',
				headers: {
					'Content-Type': 'application/xml;',
					'Accept': 'application/json, text/plain, */*',
					'csrf-token': 'undefined',
					'Referer': 'https://secure.uhcdental.com/content/dental-benefits-provider/en/secure/search-landing.html'
				},
				body: memberBody
			});
			const memberText = await memberResp.text();
			let memberKey = "";
			try {
				const memberJson = JSON.parse(memberText);
				memberKey = memberJson?.result?.consumers?.[0]?.demographics?.consumerId || "";
			} catch (e) {}

			let eligStatus = 0;
			let eligText = "";
			if (memberResp.status === 200 && memberKey) {
				const eligBody = new URLSearchParams({
					memberContrivedKey: memberKey,
					facetsIdentity: 'FXIGUESTP',
					startDate: new Date().toISOString().slice(0, 10),
					stopDate: new Date().toISOString().slice(0, 10),
					requestType: 'P',
					lapAndHcrInfoNeeded: 'Y',
					providerId: providerID
				}).toString();
				const eligResp = await fetch("https://secure.uhcdental.com/apps/dental/eligsummary", {
					method: 'POST',
					headers: {
						'Content-Type': 'application/x-www-form-urlencoded; charset=UTF-8',
						'Accept': 'application/json, text/plain, */*',
						'Referer': 'https://secure.uhcdental.com/content/dental-benefits-provider/en/secure/eligibility-summary.html'
					},
					body: eligBody
				});
				eligStatus = eligResp.status;
				eligText = await eligResp.text();
			}

			return {
				memberStatus: memberResp.status,
				memberBody: memberText,
				eligStatus: eligStatus,
				eligBody: eligText
			};
		}`, memberBody, providerID)
	}

	result, err := runFlow()
	if err != nil && strings.Contains(err.Error(), "Inspected target navigated or closed") {
		log.Printf("[UHCDental] member+eligsummary interrupted by navigation, retrying after search page settles")
		if navErr := waitForSearchLandingReady(page, 15*time.Second); navErr != nil {
			return nil, "", fmt.Errorf("browser member+eligsummary eval: %w", err)
		}
		result, err = runFlow()
	}
	if err != nil {
		return nil, "", fmt.Errorf("browser member+eligsummary eval: %w", err)
	}

	memberStatus := int(result.Value.Get("memberStatus").Int())
	memberBodyText := result.Value.Get("memberBody").Str()
	if memberStatus != 200 {
		snippet := memberBodyText
		if len(snippet) > 400 {
			snippet = snippet[:400]
		}
		log.Printf("[UHCDental] browser member search body: %s", snippet)
		return nil, "", fmt.Errorf("browser member search HTTP %d", memberStatus)
	}

	mi, err := uhcapi.ParseMemberSearchBodyForDOB([]byte(memberBodyText), dob)
	if err != nil {
		return nil, "", fmt.Errorf("parse browser member search (subscriberID=%s): %w", subscriberID, err)
	}
	eligStatus := int(result.Value.Get("eligStatus").Int())
	eligBodyText := result.Value.Get("eligBody").Str()
	if eligStatus != 200 {
		snippet := eligBodyText
		if len(snippet) > 400 {
			snippet = snippet[:400]
		}
		log.Printf("[UHCDental] browser eligsummary body: %s", snippet)
		return mi, "", fmt.Errorf("browser eligsummary HTTP %d", eligStatus)
	}

	var payload struct {
		Result struct {
			EligibilitySummary struct {
				ProductID string `json:"productId"`
			} `json:"eligibilitySummary"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(eligBodyText), &payload); err != nil {
		return mi, "", fmt.Errorf("decode browser eligsummary: %w", err)
	}
	productID := strings.TrimSpace(payload.Result.EligibilitySummary.ProductID)
	if productID == "" {
		snippet := eligBodyText
		if len(snippet) > 400 {
			snippet = snippet[:400]
		}
		log.Printf("[UHCDental] browser eligsummary body missing productId: %s", snippet)
		return mi, "", fmt.Errorf("browser eligsummary productId missing")
	}
	return mi, productID, nil
}

func (s *Session) SearchMemberAndEligSummaryViaGUI(subscriberID, dob, serviceDate string) (*uhcapi.MemberInfo, string, error) {
	page := s.inner.Page
	if err := s.navigateToSearchLanding(); err != nil {
		return nil, "", fmt.Errorf("navigate to search-landing: %w", err)
	}

	result, err := page.Eval(`async (subscriberID, dob, serviceDate) => {
		function setValue(id, value) {
			const el = document.getElementById(id);
			if (!el) {
				throw new Error("element not found: " + id);
			}
			el.focus();
			el.value = value;
			el.dispatchEvent(new Event('input', { bubbles: true }));
			el.dispatchEvent(new Event('change', { bubbles: true }));
			el.dispatchEvent(new Event('blur', { bubbles: true }));
		}

		const originalFetch = window.fetch.bind(window);
		const captured = {
			memberStatus: 0,
			memberBody: "",
			eligStatus: 0,
			eligBody: ""
		};

		window.fetch = async (...args) => {
			const response = await originalFetch(...args);
			try {
				const url = String(args[0]);
				if (url.includes("/apps/dental/member") || url.includes("/apps/dental/eligsummary")) {
					const cloned = response.clone();
					const body = await cloned.text();
					if (url.includes("/apps/dental/member")) {
						captured.memberStatus = response.status;
						captured.memberBody = body;
					}
					if (url.includes("/apps/dental/eligsummary")) {
						captured.eligStatus = response.status;
						captured.eligBody = body;
					}
				}
			} catch (e) {}
			return response;
		};

		try {
			setValue('serviceDate', serviceDate);
			setValue('eligMemDob', dob);
			setValue('eligSubsId', subscriberID);

			const btn = document.querySelector(`+"`"+`input[type="button"][value="search"]`+"`"+`);
			if (!btn) {
				throw new Error("search button not found");
			}
			btn.click();

			const started = Date.now();
			while (Date.now() - started < 20000) {
				if (captured.memberBody && captured.eligBody) {
					return captured;
				}
				await new Promise(resolve => setTimeout(resolve, 100));
			}
			throw new Error("timed out waiting for GUI member/eligsummary responses");
		} finally {
			window.fetch = originalFetch;
		}
	}`, subscriberID, dob, serviceDate)
	if err != nil {
		return nil, "", fmt.Errorf("gui search submit: %w", err)
	}

	memberStatus := int(result.Value.Get("memberStatus").Int())
	memberBody := result.Value.Get("memberBody").Str()
	eligStatus := int(result.Value.Get("eligStatus").Int())
	eligBody := result.Value.Get("eligBody").Str()

	if memberStatus != 200 {
		snippet := memberBody
		if len(snippet) > 400 {
			snippet = snippet[:400]
		}
		log.Printf("[UHCDental] gui member search body: %s", snippet)
		return nil, "", fmt.Errorf("gui member search HTTP %d", memberStatus)
	}

	mi, err := uhcapi.ParseMemberSearchBodyForDOB([]byte(memberBody), dob)
	if err != nil {
		return nil, "", fmt.Errorf("parse gui member search (subscriberID=%s): %w", subscriberID, err)
	}
	if eligStatus != 200 {
		snippet := eligBody
		if len(snippet) > 400 {
			snippet = snippet[:400]
		}
		log.Printf("[UHCDental] gui eligsummary body: %s", snippet)
		return mi, "", fmt.Errorf("gui eligsummary HTTP %d", eligStatus)
	}

	var payload struct {
		Result struct {
			EligibilitySummary struct {
				ProductID string `json:"productId"`
			} `json:"eligibilitySummary"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(eligBody), &payload); err != nil {
		return mi, "", fmt.Errorf("decode gui eligsummary: %w", err)
	}
	productID := strings.TrimSpace(payload.Result.EligibilitySummary.ProductID)
	if productID == "" {
		snippet := eligBody
		if len(snippet) > 400 {
			snippet = snippet[:400]
		}
		log.Printf("[UHCDental] gui eligsummary body missing productId: %s", snippet)
		return mi, "", fmt.Errorf("gui eligsummary productId missing")
	}
	return mi, productID, nil
}

func (s *Session) FetchEligSummaryViaBrowser(memberContrivedKey, isoDate, providerID string) (string, error) {
	page := s.inner.Page
	if err := s.navigateToSearchLanding(); err != nil {
		return "", fmt.Errorf("navigate to search-landing: %w", err)
	}

	postBody := "memberContrivedKey=" + memberContrivedKey +
		"&facetsIdentity=FXIGUESTP" +
		"&startDate=" + isoDate +
		"&stopDate=" + isoDate +
		"&requestType=P" +
		"&lapAndHcrInfoNeeded=Y" +
		"&providerId=" + providerID

	result, err := page.Eval(`async (url, body) => {
		const r = await fetch(url, {
			method: 'POST',
			headers: {
				'Content-Type': 'application/x-www-form-urlencoded; charset=UTF-8',
				'Accept': 'application/json, text/plain, */*',
				'Referer': 'https://secure.uhcdental.com/content/dental-benefits-provider/en/secure/eligibility-summary.html'
			},
			body: body
		});
		return { status: r.status, body: await r.text() };
	}`, "https://secure.uhcdental.com/apps/dental/eligsummary", postBody)
	if err != nil && strings.Contains(err.Error(), "Inspected target navigated or closed") {
		log.Printf("[UHCDental] browser eligsummary interrupted by navigation, retrying after search page settles")
		if navErr := waitForSearchLandingReady(page, 15*time.Second); navErr != nil {
			return "", fmt.Errorf("browser eligsummary eval: %w", err)
		}
		result, err = page.Eval(`async (url, body) => {
			const r = await fetch(url, {
				method: 'POST',
				headers: {
					'Content-Type': 'application/x-www-form-urlencoded; charset=UTF-8',
					'Accept': 'application/json, text/plain, */*',
					'Referer': 'https://secure.uhcdental.com/content/dental-benefits-provider/en/secure/eligibility-summary.html'
				},
				body: body
			});
			return { status: r.status, body: await r.text() };
		}`, "https://secure.uhcdental.com/apps/dental/eligsummary", postBody)
	}
	if err != nil {
		return "", fmt.Errorf("browser eligsummary eval: %w", err)
	}

	status := int(result.Value.Get("status").Int())
	bodyStr := result.Value.Get("body").Str()
	if status != 200 {
		snippet := bodyStr
		if len(snippet) > 400 {
			snippet = snippet[:400]
		}
		log.Printf("[UHCDental] browser eligsummary body: %s", snippet)
		return "", fmt.Errorf("browser eligsummary HTTP %d", status)
	}

	var payload struct {
		Result struct {
			EligibilitySummary struct {
				ProductID string `json:"productId"`
			} `json:"eligibilitySummary"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(bodyStr), &payload); err != nil {
		return "", fmt.Errorf("decode browser eligsummary: %w", err)
	}

	productID := strings.TrimSpace(payload.Result.EligibilitySummary.ProductID)
	if productID == "" {
		snippet := bodyStr
		if len(snippet) > 400 {
			snippet = snippet[:400]
		}
		log.Printf("[UHCDental] browser eligsummary body missing productId: %s", snippet)
		return "", fmt.Errorf("browser eligsummary productId missing")
	}

	return productID, nil
}

func waitForSearchLandingReady(page *rod.Page, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		info, err := page.Info()
		if err == nil && info != nil && strings.Contains(info.URL, "search-landing") {
			ready, evalErr := page.Eval(`() => {
				const serviceDate = document.querySelector('#serviceDate');
				const dob = document.querySelector('#eligMemDob');
				const subscriberID = document.querySelector('#eligSubsId');
				return !!(serviceDate && dob && subscriberID);
			}`)
			if evalErr == nil && ready.Value.Bool() {
				time.Sleep(300 * time.Millisecond)
				return nil
			}
		}
		time.Sleep(250 * time.Millisecond)
	}
	return fmt.Errorf("timed out waiting for search-landing to settle")
}

func (s *Session) ResetToSearchLanding() error {
	if s == nil || s.inner == nil || s.inner.Page == nil {
		return fmt.Errorf("browser session is not initialized")
	}
	page := s.inner.Page
	if err := page.Navigate(dashboardURL); err != nil {
		return fmt.Errorf("navigate to dashboard: %w", err)
	}
	_ = page.Timeout(10 * time.Second).WaitLoad()
	return s.navigateToSearchLanding()
}

// Close closes the browser session.
func (s *Session) Close() error {
	if s.inner != nil {
		return s.inner.Close()
	}
	return nil
}
