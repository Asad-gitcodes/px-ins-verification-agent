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

const loginURL = "https://provider.deltadental.com/dashboard/"

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
		return nil, fmt.Errorf("stat Delta Dental WA auth session: %w", statErr)
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

func (s *Session) StorageStatePath() string {
	if s == nil {
		return ""
	}
	return s.storageStatePath
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

// Login authenticates against the Delta Dental provider portal.
//
// 3-step sequence:
//  1. Enter username in #usernameInput → click #verify-user (Next).
//  2. Enter password in #password → click #btn-login (SIGN IN).
//  3. If MFA fires: fill input[aria-label="Confirmation Code"] → click Confirm.
//
// After login, navigates to portal.deltadental.com so fetch() API calls
// are same-origin and carry valid session cookies.
func (s *Session) Login(input payers.SessionInput) error {
	if s == nil || s.browser == nil || s.browser.Page == nil {
		return fmt.Errorf("browser session is not initialized")
	}
	page := s.browser.Page

	log.Printf("[DeltaDentalWA] navigating to %s", loginURL)
	if err := page.Navigate(loginURL); err != nil {
		return fmt.Errorf("navigate to Delta Dental WA login: %w", err)
	}
	_ = page.Timeout(10 * time.Second).WaitLoad()

	// Fast path: session cookie still valid.
	if isOnDashboard(page) {
		log.Printf("[DeltaDentalWA] session still active, navigating to portal home")
		if err := s.browser.SaveStorageState(s.storageStatePath); err != nil {
			return err
		}
		return s.navigateToPortalHome(page)
	}

	// Step 1: username → Next.
	log.Printf("[DeltaDentalWA] filling username")
	usernameEl, err := page.Timeout(20 * time.Second).Element(`#usernameInput`)
	if err != nil {
		return fmt.Errorf("Delta Dental WA #usernameInput not found: %w", err)
	}
	if err := usernameEl.Input(input.Credential.Username); err != nil {
		return fmt.Errorf("fill Delta Dental WA username: %w", err)
	}

	nextBtn, err := page.Timeout(10 * time.Second).Element(`#verify-user`)
	if err != nil {
		return fmt.Errorf("Delta Dental WA #verify-user (Next) not found: %w", err)
	}
	if err := nextBtn.Click(proto.InputMouseButtonLeft, 1); err != nil {
		return fmt.Errorf("click Delta Dental WA Next: %w", err)
	}
	time.Sleep(3 * time.Second)
	log.Printf("[DeltaDentalWA] after Next click, URL=%s", currentURL(page))

	// Step 2: password → SIGN IN.
	log.Printf("[DeltaDentalWA] filling password")
	passwordEl, err := page.Timeout(30 * time.Second).Element(`#password`)
	if err != nil {
		return fmt.Errorf("Delta Dental WA #password not found: %w", err)
	}
	if err := passwordEl.Click(proto.InputMouseButtonLeft, 1); err != nil {
		return fmt.Errorf("click Delta Dental WA password field: %w", err)
	}
	if err := passwordEl.Input(input.Password); err != nil {
		return fmt.Errorf("fill Delta Dental WA password: %w", err)
	}

	signInBtn, err := page.Timeout(10 * time.Second).Element(`#btn-login`)
	if err != nil {
		return fmt.Errorf("Delta Dental WA #btn-login (SIGN IN) not found: %w", err)
	}
	if err := signInBtn.Click(proto.InputMouseButtonLeft, 1); err != nil {
		return fmt.Errorf("click Delta Dental WA SIGN IN: %w", err)
	}

	// Step 3: MFA if triggered.
	if waitForMFAPrompt(page, 10*time.Second) {
		log.Printf("[DeltaDentalWA] MFA prompt detected — handling")
		if err := handleMFA(page, input); err != nil {
			return fmt.Errorf("Delta Dental WA MFA: %w", err)
		}
	}

	if err := waitForDashboard(page, 90*time.Second); err != nil {
		return err
	}

	if err := s.browser.SaveStorageState(s.storageStatePath); err != nil {
		return err
	}
	log.Printf("[DeltaDentalWA] login complete: %s", currentURL(page))

	// Navigate to portal.deltadental.com for same-origin API access.
	return s.navigateToPortalHome(page)
}

func (s *Session) navigateToPortalHome(page *rod.Page) error {
	const portalHomeURL = "https://portal.deltadental.com/portal/home"
	log.Printf("[DeltaDentalWA] navigating to portal home for API context")
	if err := page.Navigate(portalHomeURL); err != nil {
		log.Printf("[DeltaDentalWA] portal home navigation failed (non-fatal): %v", err)
		return nil
	}
	_ = page.Timeout(15 * time.Second).WaitLoad()
	log.Printf("[DeltaDentalWA] portal home URL=%s", currentURL(page))
	return nil
}

func isOnDashboard(page *rod.Page) bool {
	u := currentURL(page)
	return strings.Contains(u, "provider.deltadental.com/dashboard") ||
		strings.Contains(u, "portal.deltadental.com/portal")
}

func waitForMFAPrompt(page *rod.Page, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if isOnDashboard(page) {
			return false
		}
		if waitVisible(page, `input[aria-label="Confirmation Code"]`, 500*time.Millisecond) {
			return true
		}
		time.Sleep(300 * time.Millisecond)
	}
	return false
}

func waitForDashboard(page *rod.Page, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if isOnDashboard(page) {
			_ = page.Timeout(5 * time.Second).WaitLoad()
			return nil
		}
		time.Sleep(500 * time.Millisecond)
	}
	return fmt.Errorf("Delta Dental WA login did not reach dashboard after %s: last URL=%s", timeout, currentURL(page))
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
