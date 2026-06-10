// Package browser implements the browser session for the newpayer payer.
//
// WHAT TO FILL IN:
//   - loginURL / homeURL constants below
//   - isLoggedIn()  — selector that proves you're on the authenticated portal
//   - fillCredentials() — username/password CSS selectors and submit button
//   - ProbePatient() — the actual member search + data extraction logic
//
// MFA: handled in mfa.go. Set input.Credential.MFAMethod = "sms" | "email" | "none".

package browser

import (
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/proto"

	sharedbrowser "insurance-benefit-agent-go/internal/browser"
	"insurance-benefit-agent-go/internal/models"
	"insurance-benefit-agent-go/internal/payers"
)

// TODO(newpayer): Set these to the actual portal URLs.
const (
	loginURL = "https://portal.newpayer.example.com/login"
	homeURL  = "https://portal.newpayer.example.com/home"
)

// Session wraps a go-rod browser session for this payer.
type Session struct {
	browser          *sharedbrowser.Session
	storageStatePath string
}

// Launch opens a Chromium instance (headless in prod, visible locally).
// It restores a saved auth cookie file if one exists so that already-logged-in
// runs skip the credential form entirely.
func Launch(input payers.SessionInput) (*Session, error) {
	storageStatePath := storageStatePathFor(input)
	browserSession, err := sharedbrowser.Launch(sharedbrowser.LaunchOptions{
		StorageStatePath: storageStatePath,
		Headless:         input.Headless,
	})
	if err != nil {
		return nil, err
	}
	return &Session{
		browser:          browserSession,
		storageStatePath: storageStatePath,
	}, nil
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

// Login navigates to the portal and resolves one of three states:
//  1. Cached session — already authenticated, skip the form.
//  2. Credential form — fill username + password, submit.
//  3. MFA challenge — delegate to handleMFA() in mfa.go.
//
// On success it saves the new auth state to disk for the next run.
func (s *Session) Login(input payers.SessionInput) error {
	if s == nil || s.browser == nil || s.browser.Page == nil {
		return fmt.Errorf("NewPayer browser session not initialized")
	}
	page := s.browser.Page

	log.Printf("[NewPayer] navigating to %s", loginURL)
	if err := page.Navigate(loginURL); err != nil {
		return fmt.Errorf("navigate to NewPayer login: %w", err)
	}
	_ = page.Timeout(15 * time.Second).WaitLoad()

	// Fast path: session cookie is still valid.
	if isLoggedIn(page) {
		log.Printf("[NewPayer] session still active, skipping login")
		return s.browser.SaveStorageState(s.storageStatePath)
	}

	// Slow path: fill credential form.
	if err := fillCredentials(page, input); err != nil {
		return err
	}

	// Handle MFA if the portal asks for it.
	if waitForMFAChallenge(page, 10*time.Second) {
		if err := handleMFA(page, input); err != nil {
			return fmt.Errorf("NewPayer MFA: %w", err)
		}
	}

	// Wait for the authenticated home page.
	if err := waitForHome(page, 90*time.Second); err != nil {
		return err
	}

	if err := s.browser.SaveStorageState(s.storageStatePath); err != nil {
		return err
	}
	log.Printf("[NewPayer] login complete: %s", currentURL(page))
	return nil
}

// isLoggedIn returns true when the page shows authenticated portal content.
// TODO(newpayer): Replace the selector with one that only appears when logged in
// (e.g. a nav element, an "Eligibility" link, or the portal dashboard heading).
func isLoggedIn(page *rod.Page) bool {
	if page == nil {
		return false
	}
	// Example: `#portal-nav`, `.eligibility-search-form`, `[data-testid="dashboard"]`
	_, err := page.Timeout(2 * time.Second).Element(`TODO_LOGGED_IN_SELECTOR`)
	return err == nil
}

// fillCredentials locates and fills the username + password fields, then submits.
// TODO(newpayer): Replace every selector comment with the actual CSS selector for
// this portal. Open the portal in Chrome DevTools → Elements → right-click → Copy selector.
func fillCredentials(page *rod.Page, input payers.SessionInput) error {
	// ── Username ──────────────────────────────────────────────────────────────
	// TODO(newpayer): replace `#username` with the real selector.
	usernameEl, err := page.Timeout(20 * time.Second).Element(`#username`)
	if err != nil {
		return fmt.Errorf("NewPayer username field not found: %w", err)
	}
	if err := usernameEl.Input(input.Credential.Username); err != nil {
		return fmt.Errorf("fill NewPayer username: %w", err)
	}

	// ── Password ──────────────────────────────────────────────────────────────
	// TODO(newpayer): replace `#password` with the real selector.
	passwordEl, err := page.Timeout(10 * time.Second).Element(`#password`)
	if err != nil {
		return fmt.Errorf("NewPayer password field not found: %w", err)
	}
	if err := passwordEl.Click(proto.InputMouseButtonLeft, 1); err != nil {
		return fmt.Errorf("click NewPayer password field: %w", err)
	}
	if err := passwordEl.Input(input.Password); err != nil {
		return fmt.Errorf("fill NewPayer password: %w", err)
	}

	// ── Submit ────────────────────────────────────────────────────────────────
	// TODO(newpayer): replace `#login-btn` with the real submit button selector.
	submitEl, err := page.Timeout(10 * time.Second).Element(`#login-btn`)
	if err != nil {
		return fmt.Errorf("NewPayer login button not found: %w", err)
	}
	if err := submitEl.Click(proto.InputMouseButtonLeft, 1); err != nil {
		return fmt.Errorf("click NewPayer login button: %w", err)
	}
	return nil
}

// ProbePatient navigates to the member search, looks up a single patient, and
// returns the raw data needed to build the eligibility report.
//
// Return (nil, nil) to signal "patient not found without an error" — the caller
// will record it as "Unable to Determine".
//
// TODO(newpayer): Implement the full search-and-scrape flow here.
// Typical steps:
//  1. Navigate to the eligibility / member search page.
//  2. Clear previous results (if any).
//  3. Enter subscriber ID / member name / DOB from appt.
//  4. Submit the search form and wait for results.
//  5. Open the correct result row.
//  6. Scrape all plan/benefit fields from the detail page.
//  7. Return the scraped data as a *RawProbeData (defined in adapter.go).
//
// Note: this function lives in the browser package but uses the adapter's type
// via an import. Alternatively, you can define RawProbeData here and re-export it.
func (s *Session) ProbePatient(appt models.Appointment) (any, error) {
	page := s.browser.Page

	// TODO(newpayer): navigate to the eligibility search page.
	// Example:
	//   if err := page.Navigate("https://portal.newpayer.example.com/eligibility"); err != nil {
	//       return nil, fmt.Errorf("navigate to eligibility search: %w", err)
	//   }
	//   _ = page.Timeout(10*time.Second).WaitLoad()

	// TODO(newpayer): fill search form and submit.
	// Example using subscriber ID:
	//   el, err := page.Timeout(10*time.Second).Element(`#subscriber-id`)
	//   if err != nil { return nil, fmt.Errorf("subscriber-id field: %w", err) }
	//   _ = el.Input(appt.SubscriberID)
	//   submitEl, err := page.Timeout(5*time.Second).Element(`#search-btn`)
	//   if err != nil { return nil, fmt.Errorf("search submit: %w", err) }
	//   _ = submitEl.Click(proto.InputMouseButtonLeft, 1)

	// TODO(newpayer): wait for results and detect "not found" state.
	// Example:
	//   notFound, _ := page.Timeout(5*time.Second).Element(`.no-results-message`)
	//   if notFound != nil { return nil, nil }

	// TODO(newpayer): scrape the data from the results / detail page and return it.
	// Return a *RawProbeData (defined in adapter.go) with all fields populated.

	_ = page // suppress unused warning until implemented
	_ = appt
	return nil, fmt.Errorf("NewPayer ProbePatient not yet implemented")
}

// ── internal helpers ──────────────────────────────────────────────────────────

func waitForHome(page *rod.Page, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if isLoggedIn(page) {
			return nil
		}
		time.Sleep(500 * time.Millisecond)
	}
	return fmt.Errorf("NewPayer login did not reach home page: last URL=%s", currentURL(page))
}

// waitForMFAChallenge returns true if the portal shows an MFA prompt within timeout.
// TODO(newpayer): Replace the selector with whatever MFA input this portal uses.
func waitForMFAChallenge(page *rod.Page, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if isLoggedIn(page) {
			return false
		}
		// Common OTP input selectors — adjust or add portal-specific ones:
		if waitVisible(page,
			`input[autocomplete="one-time-code"], input[name*="code" i], input[id*="otp" i], input[inputmode="numeric"]`,
			500*time.Millisecond) {
			return true
		}
		// TODO(newpayer): add any portal-specific MFA indicator selector here.
		time.Sleep(300 * time.Millisecond)
	}
	return false
}

func waitVisible(page *rod.Page, selector string, timeout time.Duration) bool {
	if page == nil {
		return false
	}
	el, err := page.Timeout(timeout).Element(selector)
	if err != nil {
		return false
	}
	return el.WaitVisible() == nil
}

func currentURL(page *rod.Page) string {
	if page == nil {
		return ""
	}
	info, err := page.Info()
	if err != nil || info == nil {
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
	var b strings.Builder
	for _, r := range strings.ToLower(value) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		}
	}
	if b.Len() == 0 {
		return "payer"
	}
	return b.String()
}

