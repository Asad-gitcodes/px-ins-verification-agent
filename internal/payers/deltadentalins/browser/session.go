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
	// Step-1 login page — enter username only.
	loginURL = "https://provider.deltadental.com/dashboard/"
)

type Session struct {
	browser          *sharedbrowser.Session
	storageStatePath string
	hasStorageState  bool
}

// Launch opens a Chromium instance and loads the saved session cookies (if any).
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

// Login authenticates against the new Delta Dental provider portal.
//
// The portal uses a 3-step sequence:
//  1. Enter username in #usernameInput → click #verify-user (Next).
//  2. Enter password in #password → click #btn-login (SIGN IN).
//     (#username is the pre-filled readonly display of the chosen user — skip it.)
//  3. If MFA fires: fill input[aria-label="Confirmation Code"] → click Confirm.
//
// On success, saves the auth cookie state to disk so the next run reuses the
// session and skips the login form entirely.
func (s *Session) Login(input payers.SessionInput) error {
	if s == nil || s.browser == nil || s.browser.Page == nil {
		return fmt.Errorf("browser session is not initialized")
	}
	page := s.browser.Page

	log.Printf("[DeltaDental] navigating to %s", loginURL)
	if err := page.Navigate(loginURL); err != nil {
		return fmt.Errorf("navigate to Delta Dental login: %w", err)
	}
	_ = page.Timeout(10 * time.Second).WaitLoad()

	// ── Fast path: session cookie still valid ─────────────────────────────────
	if isOnDashboard(page) {
		log.Printf("[DeltaDental] session still active, skipping login")
		return s.browser.SaveStorageState(s.storageStatePath)
	}

	// ── Step 1: username → Next ───────────────────────────────────────────────
	log.Printf("[DeltaDental] filling username")
	usernameEl, err := page.Timeout(20 * time.Second).Element(`#usernameInput`)
	if err != nil {
		return fmt.Errorf("Delta Dental #usernameInput not found: %w", err)
	}
	if err := usernameEl.Input(input.Credential.Username); err != nil {
		return fmt.Errorf("fill Delta Dental username: %w", err)
	}

	nextBtn, err := page.Timeout(10 * time.Second).Element(`#verify-user`)
	if err != nil {
		return fmt.Errorf("Delta Dental #verify-user (Next) not found: %w", err)
	}
	if err := nextBtn.Click(proto.InputMouseButtonLeft, 1); err != nil {
		return fmt.Errorf("click Delta Dental Next: %w", err)
	}
	time.Sleep(2 * time.Second)

	// ── Step 2: password → SIGN IN ────────────────────────────────────────────
	// After clicking Next the portal loads a second form with #password.
	// #username on this page is read-only (pre-filled) — do not touch it.
	log.Printf("[DeltaDental] filling password")
	passwordEl, err := page.Timeout(30 * time.Second).Element(`#password`)
	if err != nil {
		return fmt.Errorf("Delta Dental #password not found: %w", err)
	}
	if err := passwordEl.Click(proto.InputMouseButtonLeft, 1); err != nil {
		return fmt.Errorf("click Delta Dental password field: %w", err)
	}
	if err := passwordEl.Input(input.Password); err != nil {
		return fmt.Errorf("fill Delta Dental password: %w", err)
	}

	signInBtn, err := page.Timeout(10 * time.Second).Element(`#btn-login`)
	if err != nil {
		return fmt.Errorf("Delta Dental #btn-login (SIGN IN) not found: %w", err)
	}
	if err := signInBtn.Click(proto.InputMouseButtonLeft, 1); err != nil {
		return fmt.Errorf("click Delta Dental SIGN IN: %w", err)
	}

	// ── Step 3: MFA if triggered ──────────────────────────────────────────────
	if waitForMFAPrompt(page, 10*time.Second) {
		log.Printf("[DeltaDental] MFA prompt detected — handling")
		if err := handleMFA(page, input); err != nil {
			return fmt.Errorf("Delta Dental MFA: %w", err)
		}
	}

	// ── Wait for authenticated dashboard ──────────────────────────────────────
	if err := waitForDashboard(page, 90*time.Second); err != nil {
		return err
	}

	if err := s.browser.SaveStorageState(s.storageStatePath); err != nil {
		return err
	}
	log.Printf("[DeltaDental] login complete: %s", currentURL(page))
	return nil
}

// ── helpers ───────────────────────────────────────────────────────────────────

// isOnDashboard returns true when the session is authenticated and on the portal.
func isOnDashboard(page *rod.Page) bool {
	u := currentURL(page)
	return strings.Contains(u, "provider.deltadental.com/dashboard") ||
		strings.Contains(u, "portal.deltadental.com/portal")
}

// waitForMFAPrompt returns true if the Confirmation Code input appears within timeout.
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

// waitForDashboard polls until the browser lands on the authenticated dashboard.
func waitForDashboard(page *rod.Page, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if isOnDashboard(page) {
			_ = page.Timeout(5 * time.Second).WaitLoad()
			return nil
		}
		time.Sleep(500 * time.Millisecond)
	}
	return fmt.Errorf("Delta Dental login did not reach dashboard after %s: last URL=%s", timeout, currentURL(page))
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
