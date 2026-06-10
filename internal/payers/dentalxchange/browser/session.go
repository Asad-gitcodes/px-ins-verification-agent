package browser

import (
	"context"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"time"
	"unicode"

	sharedbrowser "insurance-benefit-agent-go/internal/browser"
	"insurance-benefit-agent-go/internal/models"
	"insurance-benefit-agent-go/internal/payers"
	dxapi "insurance-benefit-agent-go/internal/payers/dentalxchange/api"

	"github.com/go-rod/rod"
	rodInput "github.com/go-rod/rod/lib/input"
	"github.com/go-rod/rod/lib/proto"
)

const (
	loginURL       = "https://register.dentalxchange.com/reg/login?0"
	eligibilityURL = "https://claimconnect.dentalxchange.com/dci/eligibility/EligSearchPage"
)

type Session struct {
	browser          *sharedbrowser.Session
	storageStatePath string
}

func Launch(input payers.SessionInput) (*Session, error) {
	storageStatePath := storageStatePathFor(input)
	browserSession, err := sharedbrowser.Launch(sharedbrowser.LaunchOptions{
		StorageStatePath: storageStatePath,
		Headless:         input.Headless,
	})
	if err != nil {
		return nil, err
	}
	return &Session{browser: browserSession, storageStatePath: storageStatePath}, nil
}

func (s *Session) Close() error {
	if s == nil || s.browser == nil {
		return nil
	}
	return s.browser.Close()
}

func (s *Session) Page() *rod.Page {
	if s == nil || s.browser == nil {
		return nil
	}
	return s.browser.Page
}

func (s *Session) Browser() *rod.Browser {
	if s == nil || s.browser == nil {
		return nil
	}
	return s.browser.Browser
}

func (s *Session) CurrentURL() string {
	return currentURL(s.Page())
}

func (s *Session) OpenEligibilitySearch() error {
	page := s.Page()
	if page == nil {
		return fmt.Errorf("browser session is not initialized")
	}
	if err := page.Navigate(eligibilityURL); err != nil {
		return fmt.Errorf("navigate to ClaimConnect eligibility page: %w", err)
	}
	_ = page.Timeout(10 * time.Second).WaitLoad()
	return waitForEligibilityReady(page, 60*time.Second)
}

func (s *Session) SubmitEligibility(ctx context.Context, appt models.Appointment, search dxapi.SearchRequest) (dxapi.PageSnapshot, dxapi.PageSnapshot, error) {
	page := s.Page()
	if page == nil {
		return dxapi.PageSnapshot{}, dxapi.PageSnapshot{}, fmt.Errorf("browser session is not initialized")
	}
	if err := s.OpenEligibilitySearch(); err != nil {
		return dxapi.PageSnapshot{}, dxapi.PageSnapshot{}, err
	}
	if err := fillEligibilitySearch(page, appt, search); err != nil {
		return dxapi.PageSnapshot{}, dxapi.PageSnapshot{}, err
	}
	fillSubscriberSection(page, appt, search)
	// Wicket AJAX (triggered by relationship change) may re-render the relationship dropdown
	// and reset it back to Self. Re-apply the correct value silently — no change event so we
	// don't trigger another AJAX round-trip.
	reapplyRelationship(page, appt)
	if err := clickContinue(page); err != nil {
		return dxapi.PageSnapshot{}, dxapi.PageSnapshot{}, err
	}
	if err := waitForPageText(ctx, page, 45*time.Second, "View Benefits", "Patient Details", "Benefit Search", "Eligibility Response", "Search Results", "Please fix the following"); err != nil {
		return dxapi.PageSnapshot{}, dxapi.PageSnapshot{}, fmt.Errorf("wait for eligibility result: %w", err)
	}
	if msg := extractClaimConnectError(page); msg != "" {
		return dxapi.PageSnapshot{}, dxapi.PageSnapshot{}, fmt.Errorf("payer rejected: %s", msg)
	}
	// Some payers (e.g. Delta Dental) return a "Search Results" list when the subscriber has
	// multiple members. Click the row that matches the appointment patient.
	if isSearchResultsPage(page) {
		if err := clickPatientInResults(page, appt); err != nil {
			return dxapi.PageSnapshot{}, dxapi.PageSnapshot{}, fmt.Errorf("select patient from search results: %w", err)
		}
		if err := waitForPageText(ctx, page, 30*time.Second, "View Benefits", "Patient Details", "Benefit Search", "Eligibility Response"); err != nil {
			return dxapi.PageSnapshot{}, dxapi.PageSnapshot{}, fmt.Errorf("wait for eligibility after patient select: %w", err)
		}
	}
	eligibilityPage := snapshot(page, "eligibilityResult")
	if err := clickViewBenefits(page); err != nil {
		return eligibilityPage, dxapi.PageSnapshot{}, err
	}
	if err := waitForBenefitsLoaded(ctx, page, 60*time.Second); err != nil {
		return eligibilityPage, dxapi.PageSnapshot{}, fmt.Errorf("wait for benefits result: %w", err)
	}
	return eligibilityPage, snapshot(page, "benefits"), nil
}

func (s *Session) Login(input payers.SessionInput) error {
	if s == nil || s.browser == nil || s.browser.Page == nil {
		return fmt.Errorf("browser session is not initialized")
	}
	page := s.browser.Page
	if err := page.Navigate(eligibilityURL); err != nil {
		return fmt.Errorf("navigate to ClaimConnect eligibility page: %w", err)
	}
	_ = page.Timeout(10 * time.Second).WaitLoad()

	if isEligibilityPage(page) {
		return s.save()
	}
	if !isLoginPage(page) {
		if err := page.Navigate(loginURL); err != nil {
			return fmt.Errorf("navigate to DentalXChange login: %w", err)
		}
		_ = page.Timeout(10 * time.Second).WaitLoad()
	}
	if err := tryCredentialLogin(page, input); err != nil {
		log.Printf("[DentalXChange] automatic login not completed: %v", err)
	}
	if err := waitForEligibilityReady(page, 120*time.Second); err != nil {
		return err
	}
	return s.save()
}

func (s *Session) Cookies() ([]*proto.NetworkCookie, error) {
	if s == nil || s.browser == nil || s.browser.Browser == nil {
		return nil, fmt.Errorf("browser session is not initialized")
	}
	res, err := proto.StorageGetCookies{}.Call(s.browser.Browser)
	if err != nil {
		return nil, err
	}
	return res.Cookies, nil
}

func (s *Session) save() error {
	if err := s.browser.SaveStorageState(s.storageStatePath); err != nil {
		return err
	}
	log.Printf("[DentalXChange] login/session ready: %s", currentURL(s.Page()))
	return nil
}

func tryCredentialLogin(page *rod.Page, input payers.SessionInput) error {
	if page == nil {
		return fmt.Errorf("page is nil")
	}
	time.Sleep(750 * time.Millisecond)
	username, err := firstVisible(page, []string{
		`#username1`,
		`input[name="username"]`,
		`input[name*="user" i]`,
		`input[id*="user" i]`,
		`input[type="email"]`,
		`input[autocomplete="username"]`,
	})
	if err != nil {
		return err
	}
	if err := username.Input(input.Credential.Username); err != nil {
		return fmt.Errorf("fill username: %w", err)
	}
	time.Sleep(250 * time.Millisecond)
	password, err := firstVisible(page, []string{
		`#password9`,
		`input[name="password"]`,
		`input[type="password"]`,
		`input[name*="pass" i]`,
		`input[id*="pass" i]`,
	})
	if err != nil {
		return err
	}
	if err := password.Input(input.Password); err != nil {
		return fmt.Errorf("fill password: %w", err)
	}
	if err := waitForPasswordValue(page, 5*time.Second); err != nil {
		return err
	}
	time.Sleep(500 * time.Millisecond)
	if err := clickLoginButton(page, password); err == nil {
		return nil
	}
	button, err := firstVisible(page, []string{
		`button[type="submit"]`,
		`input[type="submit"]`,
		`button`,
	})
	if err != nil {
		return err
	}
	if err := button.Click(proto.InputMouseButtonLeft, 1); err != nil {
		if strings.Contains(err.Error(), "context canceled") {
			return nil
		}
		return fmt.Errorf("click login button: %w", err)
	}
	return nil
}

func waitForPasswordValue(page *rod.Page, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		res, err := page.Eval(`() => {
			const el = document.querySelector('#password9, input[name="password"], input[type="password"]');
			return !!(el && el.value && el.value.length > 0);
		}`)
		if err == nil && res.Value.Bool() {
			return nil
		}
		time.Sleep(200 * time.Millisecond)
	}
	return fmt.Errorf("password field did not retain value")
}

func clickLoginButton(page *rod.Page, password *rod.Element) error {
	if page == nil {
		return fmt.Errorf("page is nil")
	}
	button, err := page.Timeout(10*time.Second).ElementR(`button, input[type="submit"], a`, `(?i)log\s*in|sign\s*in`)
	if err == nil {
		if clickErr := button.Click(proto.InputMouseButtonLeft, 1); clickErr != nil {
			if strings.Contains(clickErr.Error(), "context canceled") {
				return nil
			}
			log.Printf("[DentalXChange] login button click failed; pressing Enter: %v", clickErr)
		} else {
			return nil
		}
	}
	if password != nil {
		_ = password.Focus()
	}
	if err := page.Keyboard.Press(rodInput.Enter); err != nil {
		return fmt.Errorf("press Enter on login form: %w", err)
	}
	return nil
}

func isLoginPage(page *rod.Page) bool {
	if page == nil {
		return false
	}
	u := currentURL(page)
	if !strings.Contains(u, "register.dentalxchange.com/reg/login") {
		return false
	}
	return waitVisible(page, `#username1, input[name="username"]`, 500*time.Millisecond)
}

func firstVisible(page *rod.Page, selectors []string) (*rod.Element, error) {
	for _, selector := range selectors {
		el, err := page.Timeout(3 * time.Second).Element(selector)
		if err != nil {
			continue
		}
		if visible, _ := el.Visible(); visible {
			return el, nil
		}
	}
	return nil, fmt.Errorf("no visible element found")
}

func waitVisible(page *rod.Page, selector string, timeout time.Duration) bool {
	el, err := page.Timeout(timeout).Element(selector)
	if err != nil {
		return false
	}
	visible, err := el.Visible()
	return err == nil && visible
}

func waitForEligibilityReady(page *rod.Page, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if isEligibilityPage(page) {
			_ = page.Timeout(5 * time.Second).WaitLoad()
			return nil
		}
		if isDashboardPage(page) {
			if err := openEligibilityFromDashboard(page); err != nil {
				log.Printf("[DentalXChange] dashboard eligibility navigation failed: %v", err)
			}
			time.Sleep(time.Second)
			continue
		}
		time.Sleep(time.Second)
	}
	return fmt.Errorf("ClaimConnect login did not reach eligibility page: lastURL=%s", currentURL(page))
}

func isDashboardPage(page *rod.Page) bool {
	u := currentURL(page)
	return strings.Contains(u, "register.dentalxchange.com/reg/dashboard")
}

func openEligibilityFromDashboard(page *rod.Page) error {
	if page == nil {
		return fmt.Errorf("page is nil")
	}
	if el, err := page.Timeout(2*time.Second).ElementR(`a, button, div, span`, `(?i)^Real Time Eligibility$|Real-Time Eligibility`); err == nil {
		if err := el.Click(proto.InputMouseButtonLeft, 1); err != nil && !strings.Contains(err.Error(), "context canceled") {
			return fmt.Errorf("click Real Time Eligibility: %w", err)
		}
		return nil
	}
	if el, err := page.Timeout(2*time.Second).ElementR(`a, button`, `(?i)^Eligibility$`); err == nil {
		if err := el.Click(proto.InputMouseButtonLeft, 1); err != nil && !strings.Contains(err.Error(), "context canceled") {
			return fmt.Errorf("click Eligibility menu: %w", err)
		}
		return nil
	}
	return page.Navigate(eligibilityURL)
}

func fillEligibilitySearch(page *rod.Page, appt models.Appointment, search dxapi.SearchRequest) error {
	relCode := computeRelCode(appt)

	if _, err := page.Eval(`(providerID, providerText, payerID, payerText, memberID, relationship, lastName, firstName, dob) => {
		function bySuffix(suffix) {
			return Array.from(document.querySelectorAll('[name]')).find((el) => el.name.endsWith(suffix));
		}
		function fire(el) {
			for (const name of ['input', 'change', 'blur']) {
				el.dispatchEvent(new Event(name, { bubbles: true }));
			}
			if (window.jQuery) {
				window.jQuery(el).trigger('change');
			}
		}
		function setValue(suffix, value, label) {
			const el = bySuffix(suffix);
			if (!el) {
				throw new Error('field not found: ' + suffix);
			}
			if (el.tagName === 'SELECT' && value && !Array.from(el.options).some((opt) => opt.value === value)) {
				el.add(new Option(label || value, value, true, true));
			}
			el.focus();
			el.value = value || '';
			fire(el);
		}
		setValue('billingProvider', providerID, providerText);
		setValue('payer', payerID, payerText);
		setValue('ssn', memberID, '');
		setValue('patientSubscriberRelationship', relationship || '18', '');
		setValue('patientLastName', lastName, '');
		setValue('patientFirstName', firstName, '');
		setValue('patientDob', dob, '');
		setValue('groupNum', '', '');
	}`, search.BillingProvider, search.ProviderText, search.PayerValue, search.PayerLabel, search.MemberID, relCode, appt.LName, appt.FName, search.DateOfBirth); err != nil {
		return fmt.Errorf("fill eligibility form: %w", err)
	}
	return nil
}

// fillSubscriberSection waits for the subscriber section to appear (some payers trigger it via
// Wicket AJAX when the relationship dropdown changes) then fills the required fields.
// Non-fatal: if the subscriber section is not present or not visible, it is skipped.
func fillSubscriberSection(page *rod.Page, appt models.Appointment, search dxapi.SearchRequest) {
	subFName := strings.TrimSpace(appt.SubFName)
	subLName := strings.TrimSpace(appt.SubLName)
	subDOB := normalizeDateDX(strings.TrimSpace(appt.SubDOB))
	if subFName == "" {
		subFName = strings.TrimSpace(appt.FName)
	}
	if subLName == "" {
		subLName = strings.TrimSpace(appt.LName)
	}
	if subDOB == "" {
		subDOB = search.DateOfBirth
	}

	// Wait up to 3s for Wicket AJAX to render the subscriber section.
	// Payers that don't require it simply won't inject the field — timeout is short to avoid
	// adding a 8s delay to every MetLife-style payer that never shows subscriber fields.
	el, err := page.Timeout(3 * time.Second).Element(`[name$="subscriberLastName"]`)
	if err != nil {
		return // payer does not require subscriber section
	}
	if visible, _ := el.Visible(); !visible {
		return // section present but hidden for this payer
	}

	_, _ = page.Eval(`(subLastName, subFirstName, subDob) => {
		function bySuffix(suffix) {
			return Array.from(document.querySelectorAll('[name]')).find((el) => el.name.endsWith(suffix));
		}
		function fire(el) {
			for (const name of ['input', 'change', 'blur']) {
				el.dispatchEvent(new Event(name, { bubbles: true }));
			}
		}
		function trySet(suffix, value) {
			if (!value) return;
			const el = bySuffix(suffix);
			if (!el) return;
			el.focus();
			el.value = value;
			fire(el);
		}
		trySet('subscriberLastName', subLastName);
		trySet('subscriberFirstName', subFirstName);
		trySet('subscriberDob', subDob);
	}`, subLName, subFName, subDOB)
}

// reapplyRelationship sets the relationship SELECT value without firing the change event,
// so Wicket does not trigger another AJAX round-trip that would reset it again.
func reapplyRelationship(page *rod.Page, appt models.Appointment) {
	relCode := computeRelCode(appt)
	if relCode == "18" {
		return // Self is the default; nothing to fix
	}
	_, _ = page.Eval(`(relCode) => {
		const el = Array.from(document.querySelectorAll('[name]'))
			.find(e => e.name.endsWith('patientSubscriberRelationship'));
		if (el && el.value !== relCode) {
			el.value = relCode;
		}
	}`, relCode)
}

func computeRelCode(appt models.Appointment) string {
	relCode := "18" // Self
	switch strings.TrimSpace(appt.Relationship) {
	case "1":
		relCode = "01" // Spouse
	case "2":
		relCode = "19" // Child/Dependent
	}
	if relCode == "18" {
		subFName := strings.ToLower(strings.TrimSpace(appt.SubFName))
		subLName := strings.ToLower(strings.TrimSpace(appt.SubLName))
		subDOB := strings.TrimSpace(appt.SubDOB)
		patFName := strings.ToLower(strings.TrimSpace(appt.FName))
		patLName := strings.ToLower(strings.TrimSpace(appt.LName))
		patDOB := strings.TrimSpace(appt.DOB)
		namesDiffer := (subFName != "" && subFName != patFName) || (subLName != "" && subLName != patLName)
		dobDiffers := subDOB != "" && patDOB != "" && subDOB != patDOB
		if namesDiffer || dobDiffers {
			relCode = "19"
		}
	}
	return relCode
}

func normalizeDateDX(dob string) string {
	dob = strings.TrimSpace(dob)
	layouts := []struct{ in, out string }{
		{"01-02-2006", "01/02/2006"},
		{"01/02/2006", "01/02/2006"},
		{"2006-01-02", "01/02/2006"},
	}
	for _, l := range layouts {
		if t, err := time.Parse(l.in, dob); err == nil {
			return t.Format(l.out)
		}
	}
	return dob
}

func clickContinue(page *rod.Page) error {
	if el, err := page.Timeout(10*time.Second).ElementR(`button, input, a`, `(?i)continue`); err == nil {
		if err := el.Click(proto.InputMouseButtonLeft, 1); err != nil && !strings.Contains(err.Error(), "context canceled") {
			return fmt.Errorf("click Continue: %w", err)
		}
		return nil
	}
	if _, err := page.Eval(`() => {
		const el = Array.from(document.querySelectorAll('button, input, a')).find((node) => {
			const text = (node.innerText || node.value || node.getAttribute('aria-label') || '').trim();
			return /continue/i.test(text) || (node.name || '').endsWith('actionBar:actions:0:action');
		});
		if (!el) {
			throw new Error('Continue button not found');
		}
		el.click();
	}`); err != nil {
		return fmt.Errorf("click Continue: %w", err)
	}
	return nil
}

func isSearchResultsPage(page *rod.Page) bool {
	res, err := page.Eval(`() => {
		const text = document.body ? document.body.innerText : '';
		return /search\s+results/i.test(text) && document.querySelector('table') !== null;
	}`)
	return err == nil && res.Value.Bool()
}

func clickPatientInResults(page *rod.Page, appt models.Appointment) error {
	patFName := strings.TrimSpace(appt.FName)
	patLName := strings.TrimSpace(appt.LName)
	dob := normalizeDateDX(strings.TrimSpace(appt.DOB))

	// Find the index of the correct link using JS (DOB match preferred, name fallback).
	// We get an index back and then use Rod's real mouse click for Wicket compatibility.
	res, err := page.Eval(`(firstName, lastName, dob) => {
		const links = Array.from(document.querySelectorAll('table a'));
		const fn = firstName.toUpperCase();
		const ln = lastName.toUpperCase();
		// Primary: row contains patient DOB
		if (dob) {
			for (let i = 0; i < links.length; i++) {
				const row = links[i].closest('tr');
				if (row && (row.innerText || row.textContent || '').includes(dob)) {
					return i;
				}
			}
		}
		// Fallback: link text matches first + last name
		for (let i = 0; i < links.length; i++) {
			const text = (links[i].innerText || links[i].textContent || '').trim().toUpperCase();
			if (fn && ln && text.includes(fn) && text.includes(ln)) {
				return i;
			}
		}
		return -1;
	}`, patFName, patLName, dob)
	if err != nil {
		return fmt.Errorf("search results eval: %w", err)
	}

	idx := int(res.Value.Int())
	if idx < 0 {
		return fmt.Errorf("patient not found in search results: %s %s DOB=%s", patFName, patLName, dob)
	}

	// Use Rod's real mouse click — JS link.click() is unreliable for Wicket anchor elements
	elements, err := page.Timeout(3 * time.Second).Elements("table a")
	if err != nil {
		return fmt.Errorf("table links not found: %w", err)
	}
	if idx >= len(elements) {
		return fmt.Errorf("patient link index %d out of range (total=%d)", idx, len(elements))
	}
	linkText, _ := elements[idx].Text()
	log.Printf("[DentalXChange] search results: clicking patient link %q (index=%d DOB=%s)", linkText, idx, dob)
	return elements[idx].Click(proto.InputMouseButtonLeft, 1)
}

func clickViewBenefits(page *rod.Page) error {
	if el, err := page.Timeout(10 * time.Second).Element(`a[onclick*="viewBenefitsButton"], button[onclick*="viewBenefitsButton"], input[onclick*="viewBenefitsButton"]`); err == nil {
		if err := el.ScrollIntoView(); err != nil {
			log.Printf("[DentalXChange] scroll View Benefits button failed: %v", err)
		}
		if err := el.Click(proto.InputMouseButtonLeft, 1); err != nil && !strings.Contains(err.Error(), "context canceled") {
			return fmt.Errorf("click View Benefits: %w", err)
		}
		return nil
	}
	if el, err := page.Timeout(10*time.Second).ElementR(`button, input, a`, `(?i)view\s+benefits`); err == nil {
		if err := el.ScrollIntoView(); err != nil {
			log.Printf("[DentalXChange] scroll View Benefits text button failed: %v", err)
		}
		if err := el.Click(proto.InputMouseButtonLeft, 1); err != nil && !strings.Contains(err.Error(), "context canceled") {
			return fmt.Errorf("click View Benefits: %w", err)
		}
		return nil
	}
	if _, err := page.Eval(`() => {
		const radio = document.querySelector('input[name$="searchOptionRadioGroup"][value="radio12"]');
		if (radio && !radio.checked) {
			radio.checked = true;
			radio.dispatchEvent(new Event('change', { bubbles: true }));
		}
		const el = Array.from(document.querySelectorAll('a, button, input')).find((node) => {
			const text = (node.innerText || node.value || node.getAttribute('aria-label') || '').trim();
			const onclick = node.getAttribute('onclick') || '';
			return onclick.includes('viewBenefitsButton') || /view\s+benefits/i.test(text);
		});
		if (!el) {
			throw new Error('View Benefits button not found');
		}
		el.click();
	}`); err == nil {
		return nil
	}
	if _, err := page.Eval(`() => {
		const el = Array.from(document.querySelectorAll('button, input, a')).find((node) => {
			const text = (node.innerText || node.value || node.getAttribute('aria-label') || '').trim();
			return /view\s+benefits|benefits/i.test(text) || (node.name || '').includes('viewBenefitsButton');
		});
		if (!el) {
			throw new Error('View Benefits button not found');
		}
		el.click();
	}`); err != nil {
		return fmt.Errorf("click View Benefits: %w", err)
	}
	return nil
}

func waitForPageText(ctx context.Context, page *rod.Page, timeout time.Duration, terms ...string) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		res, err := page.Eval(`(terms) => {
			const text = document.body ? document.body.innerText : '';
			return terms.some((term) => text.toLowerCase().includes(String(term).toLowerCase()));
		}`, terms)
		if err == nil && res.Value.Bool() {
			_ = page.Timeout(5 * time.Second).WaitLoad()
			return nil
		}
		time.Sleep(500 * time.Millisecond)
	}
	return fmt.Errorf("timed out waiting for page text; lastURL=%s", currentURL(page))
}

// waitForBenefitsLoaded polls for table content that is specific to the benefits page.
// It first checks that the Wicket AJAX indicator is idle (showincrementallycount=0) before
// inspecting content — this prevents a false-positive snapshot while the AJAX is still loading.
func waitForBenefitsLoaded(ctx context.Context, page *rod.Page, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var sparseResultSince time.Time
	var lastSparseSignature string
	const sparseGracePeriod = 6 * time.Second
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		res, err := page.Eval(`() => {
			const body = document.body;
			if (!body) return { ready: false };
			// Wait for Wicket AJAX to finish before checking content.
			// showincrementallycount > 0 means a request is still in flight.
			const indicator = document.querySelector('[showincrementallycount]');
			if (indicator && parseInt(indicator.getAttribute('showincrementallycount') || '0', 10) > 0) {
				return { ready: false };
			}
			const text = body.innerText || body.textContent || '';
			const lower = text.toLowerCase();
			const stillOnBenefitSearch =
				lower.includes('patient details and benefit search') &&
				lower.includes('select one of the options below to view plan benefits') &&
				!!document.querySelector('[onclick*="viewBenefitsButton"]');
			const onBenefitsResult = !stillOnBenefitSearch && /plan\s+benefits/i.test(text);
			const noBenefits = /no\s+benefits\s+(found|available)|benefits\s+not\s+available/i.test(text);
			if (noBenefits) return { ready: true, detailed: false, signature: 'no-benefits' };
			const detailedText =
				/deductibles|co-insurance|coinsurance|limitations\s+and\s+maximums|contract\s+amount|plan\s+year\s+maximum|annual\s+deductible|benefit\s+summary/i.test(text) ||
				(/in.network/i.test(text) && /out.of.network/i.test(text));
			// Benefit table has a "Service Type" column with dollar amounts
			const tables = Array.from(document.querySelectorAll('table'));
			const detailedTable = tables.some(t => {
				const cells = Array.from(t.querySelectorAll('th, td')).map(c => (c.textContent || '').trim().toLowerCase());
				return cells.some(h => h === 'service type' || h.includes('contract amount') || h.includes('remaining amount') || h === 'benefit' || h === 'in-network' || h === 'out-of-network');
			});
			if (detailedText || detailedTable) {
				return { ready: true, detailed: true, signature: String(text.length) };
			}
			if (onBenefitsResult) {
				return { ready: true, detailed: false, signature: String(text.length) + ':' + lower.slice(-300) };
			}
			return { ready: false };
		}`)
		if err == nil {
			ready := res.Value.Get("ready").Bool()
			if ready && res.Value.Get("detailed").Bool() {
				return nil
			}
			if ready {
				signature := res.Value.Get("signature").Str()
				now := time.Now()
				if signature == "" || signature != lastSparseSignature {
					lastSparseSignature = signature
					sparseResultSince = now
				} else if !sparseResultSince.IsZero() && now.Sub(sparseResultSince) >= sparseGracePeriod {
					return nil
				}
			} else {
				lastSparseSignature = ""
				sparseResultSince = time.Time{}
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	return fmt.Errorf("timed out waiting for benefit tables; lastURL=%s", currentURL(page))
}

func snapshot(page *rod.Page, step string) dxapi.PageSnapshot {
	if page == nil {
		return dxapi.PageSnapshot{FetchStep: step}
	}
	rawURL := currentURL(page)
	html := ""
	if res, err := page.Eval(`() => document.documentElement ? document.documentElement.outerHTML : ''`); err == nil {
		html = res.Value.Str()
	}
	return dxapi.SnapshotFromHTML(rawURL, html, step)
}

func isEligibilityPage(page *rod.Page) bool {
	if page == nil {
		return false
	}
	u := currentURL(page)
	if strings.Contains(u, "/dci/eligibility/EligSearchPage") || strings.Contains(u, "/dci/wicket/page") {
		return true
	}
	_, err := page.Timeout(500*time.Millisecond).ElementR(
		"body",
		"(?i)Eligibility Search|Select Billing Provider|Select a Payer|Patient Details and Benefit Search",
	)
	return err == nil
}

func currentURL(page *rod.Page) string {
	if page == nil {
		return ""
	}
	info, err := page.Info()
	if err != nil {
		return ""
	}
	return info.URL
}

func storageStatePathFor(input payers.SessionInput) string {
	return fmt.Sprintf("auth-%s-%s-slot-%s.json",
		slug(input.Payer.PayerURL),
		input.RequestedOfficeKey,
		strconv.Itoa(input.Credential.Slot),
	)
}

func slug(value string) string {
	var builder strings.Builder
	for _, r := range strings.ToLower(value) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			builder.WriteRune(r)
		}
	}
	if builder.Len() == 0 {
		return "payer"
	}
	return builder.String()
}

func HasStorageState(input payers.SessionInput) bool {
	_, err := os.Stat(storageStatePathFor(input))
	return err == nil
}

func extractClaimConnectError(page *rod.Page) string {
	res, err := page.Eval(`() => {
		const items = Array.from(document.querySelectorAll(
			'.feedbackPanel li, [class*="feedback"] li, [class*="error"] li, .alert li'
		));
		const msgs = items.map(el => (el.innerText || el.textContent || '').trim()).filter(Boolean);
		return msgs.join('; ');
	}`)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(res.Value.Str())
}
