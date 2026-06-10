package browser

import (
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"time"
	"unicode"

	sharedbrowser "insurance-benefit-agent-go/internal/browser"
	"insurance-benefit-agent-go/internal/payers"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/proto"
)

const (
	loginURL  = "https://www1.deltadentalins.com/dentists.html"
	portalURL = "https://www1.deltadentalins.com/provider-tools/v2"
)

type Session struct {
	browser          *sharedbrowser.Session
	storageStatePath string
	hasStorageState  bool
}

func Launch(input payers.SessionInput) (*Session, error) {
	storageStatePath := storageStatePathFor(input)
	_, statErr := os.Stat(storageStatePath)
	hasStorageState := statErr == nil
	if statErr != nil && !os.IsNotExist(statErr) {
		return nil, fmt.Errorf("stat Delta Dental auth session: %w", statErr)
	}

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
		hasStorageState:  hasStorageState,
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

func (s *Session) Browser() *rod.Browser {
	if s == nil || s.browser == nil {
		return nil
	}
	return s.browser.Browser
}

// Login navigates to the Delta Dental provider portal and authenticates.
// Mirrors the Node.js login() + waitForPortal() in index.js.
func (s *Session) Login(input payers.SessionInput) error {
	if s == nil || s.browser == nil || s.browser.Page == nil {
		return fmt.Errorf("browser session is not initialized")
	}
	page := s.browser.Page

	if err := page.Navigate(loginURL); err != nil {
		return fmt.Errorf("navigate to Delta Dental login: %w", err)
	}
	_ = page.Timeout(5 * time.Second).WaitLoad()

	// Dismiss cookie consent banner if present.
	if el, err := page.Timeout(3 * time.Second).Element(`#onetrust-accept-btn-handler`); err == nil {
		if visible, _ := el.Visible(); visible {
			_ = el.Click(proto.InputMouseButtonLeft, 1)
			time.Sleep(500 * time.Millisecond)
		}
	}

	// Click "Log in / register" — target by href since the link always goes to /ciam/.
	loginLink, err := page.Timeout(10 * time.Second).Element(`a[href*="/ciam/"]`)
	if err != nil {
		return fmt.Errorf("login link not found: %w", err)
	}
	if err := loginLink.Click(proto.InputMouseButtonLeft, 1); err != nil {
		return fmt.Errorf("click login link: %w", err)
	}

	// With valid session cookies Okta redirects straight to the portal — no form.
	// Poll briefly before assuming the credentials form is needed.
	if waitForPortalRedirect(page, 5*time.Second) {
		if err := s.browser.SaveStorageState(s.storageStatePath); err != nil {
			return err
		}
		return nil
	}

	// Session expired — Okta shows the username/password form.
	usernameEl, err := page.Timeout(15 * time.Second).Element(
		`input[name="identifier"], input[autocomplete="username"], input[aria-label="Username"]`,
	)
	if err != nil {
		return fmt.Errorf("username field not found: %w", err)
	}
	if err := usernameEl.Input(input.Credential.Username); err != nil {
		return fmt.Errorf("fill username: %w", err)
	}

	// Tick "Keep me signed in" if present — Okta uses a stable data-se attribute.
	if el, err := page.Timeout(2 * time.Second).Element(`[data-se-for-name="rememberMe"]`); err == nil {
		if visible, _ := el.Visible(); visible {
			_ = el.Click(proto.InputMouseButtonLeft, 1)
		}
	}

	el, err := page.Element(`input[type="submit"][value="Next"]`)
	if err != nil {
		return fmt.Errorf("fetch next button: %w", err)
	}
	if err := el.WaitVisible(); err != nil {
		return fmt.Errorf("next button not visible: %w", err)
	}
	if err := el.Click(proto.InputMouseButtonLeft, 1); err != nil {
		return fmt.Errorf("click next button: %w", err)
	}

	// Fill password.
	passwordEl, err := page.Timeout(15 * time.Second).Element(`input[type="password"]`)
	if err != nil {
		return fmt.Errorf("password field not found: %w", err)
	}
	if err := passwordEl.Input(input.Password); err != nil {
		return fmt.Errorf("fill password: %w", err)
	}

	verifyEl, err := page.Timeout(15 * time.Second).Element(`input[type="submit"][value="Verify"]`)
	if err != nil {
		return fmt.Errorf("Verify button not found: %w", err)
	}
	if err := verifyEl.Click(proto.InputMouseButtonLeft, 1); err != nil {
		return fmt.Errorf("click Verify: %w", err)
	}

	// After Verify: MFA screen or direct portal redirect.
	state := detectPostLoginState(page)

	if state != "portal" {
		if err := handleMFA(page, state, input); err != nil {
			return fmt.Errorf("MFA: %w", err)
		}
	}

	if err := waitForProviderPortal(page, 60*time.Second); err != nil {
		return err
	}

	if err := s.browser.SaveStorageState(s.storageStatePath); err != nil {
		return err
	}
	log.Printf("[DeltaDental] login complete: %s", currentURL(page))
	return nil
}

// detectPostLoginState races between the MFA screens and a direct portal redirect.
// Returns "mfa_select", "mfa_sms", "portal", or "timeout".
func detectPostLoginState(page *rod.Page) string {
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if waitVisible(page, `.authenticator-verify-list`, 300*time.Millisecond) {
			return "mfa_select"
		}
		if waitVisible(page, `[data-se="phone_number"] a`, 300*time.Millisecond) {
			return "mfa_sms"
		}
		if isOnProviderPortal(page) {
			return "portal"
		}
	}
	return "timeout"
}

// waitForProviderPortal polls until the browser lands on the provider portal.
// Mirrors the Node.js waitForPortal() logic in index.js.
func waitForProviderPortal(page *rod.Page, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		u := currentURL(page)

		if strings.Contains(u, "/provider-tools") {
			_ = page.Timeout(5 * time.Second).WaitLoad()
			return nil
		}
		// Okta callback page — wait for JS redirect.
		if strings.Contains(u, "/ciam/login") {
			time.Sleep(time.Second)
			continue
		}

		// Interstitial with a Continue / Proceed / Next button.
		if el, err := page.Timeout(500*time.Millisecond).ElementR(`button`, `Continue|Proceed|Next`); err == nil {
			if visible, _ := el.Visible(); visible {
				_ = el.Click(proto.InputMouseButtonLeft, 1)
				_ = page.Timeout(3 * time.Second).WaitLoad()
				continue
			}
		}

		time.Sleep(time.Second)
	}
	return fmt.Errorf("Delta Dental login did not reach provider portal: last URL=%s", currentURL(page))
}

func isOnProviderPortal(page *rod.Page) bool {
	return strings.Contains(currentURL(page), "/provider-tools")
}

func waitForPortalRedirect(page *rod.Page, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if isOnProviderPortal(page) {
			return true
		}
		time.Sleep(300 * time.Millisecond)
	}
	return false
}

// clickButton finds and clicks a button whose visible text matches pattern.
func clickButton(page *rod.Page, pattern string) error {
	el, err := page.Timeout(10*time.Second).ElementR(`button`, pattern)
	if err != nil {
		return fmt.Errorf("button %q not found: %w", pattern, err)
	}
	return el.Click(proto.InputMouseButtonLeft, 1)
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

func waitVisible(page *rod.Page, selector string, timeout time.Duration) bool {
	el, err := page.Timeout(timeout).Element(selector)
	if err != nil {
		return false
	}
	visible, err := el.Visible()
	return err == nil && visible
}
