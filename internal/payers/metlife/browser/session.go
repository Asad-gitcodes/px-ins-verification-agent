package browser

import (
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/proto"

	sharedbrowser "insurance-benefit-agent-go/internal/browser"
	"insurance-benefit-agent-go/internal/payers"
)

const (
	loginURL = "https://dentalprovider.metlife.com/presignin"
	homeURL  = "https://dentalprovider.metlife.com/home"
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

func (s *Session) ExtractSessionCookies() ([]*http.Cookie, error) {
	if s == nil || s.browser == nil || s.browser.Browser == nil {
		return nil, fmt.Errorf("browser session is not initialized")
	}

	res, err := proto.StorageGetCookies{}.Call(s.browser.Browser)
	if err != nil {
		return nil, fmt.Errorf("get MetLife browser cookies: %w", err)
	}

	cookies := make([]*http.Cookie, 0, len(res.Cookies))
	for _, c := range res.Cookies {
		val := strings.Map(func(r rune) rune {
			if r == '"' || r < 0x20 || r == 0x7f {
				return -1
			}
			return r
		}, c.Value)
		cookies = append(cookies, &http.Cookie{
			Name:     c.Name,
			Value:    val,
			Domain:   c.Domain,
			Path:     "/",
			Secure:   c.Secure,
			HttpOnly: c.HTTPOnly,
		})
	}
	return cookies, nil
}

func (s *Session) Login(input payers.SessionInput) error {
	if s == nil || s.browser == nil || s.browser.Page == nil {
		return fmt.Errorf("browser session is not initialized")
	}
	page := s.browser.Page

	log.Printf("[MetLife] navigating to %s", loginURL)
	if err := page.Navigate(loginURL); err != nil {
		return fmt.Errorf("navigate to MetLife login: %w", err)
	}
	_ = page.Timeout(10 * time.Second).WaitLoad()

	if isOnHome(page) {
		log.Printf("[MetLife] session still active, skipped login")
		settleHomePage(page)
		s.StartNetworkLogger()
		return s.browser.SaveStorageState(s.storageStatePath)
	}

	if err := ensureLoginFormVisible(page); err != nil {
		return err
	}

	if isOnHome(page) {
		log.Printf("[MetLife] session still active (Sign In redirected to home), skipped login")
		settleHomePage(page)
		s.StartNetworkLogger()
		return s.browser.SaveStorageState(s.storageStatePath)
	}

	usernameEl, err := page.Timeout(20 * time.Second).Element(`#username`)
	if err != nil {
		return fmt.Errorf("MetLife username field not found: %w", err)
	}
	if err := usernameEl.Input(input.Credential.Username); err != nil {
		return fmt.Errorf("fill MetLife username: %w", err)
	}

	passwordEl, err := page.Timeout(20 * time.Second).Element(`#password`)
	if err != nil {
		return fmt.Errorf("MetLife password field not found: %w", err)
	}
	if err := passwordEl.Click(proto.InputMouseButtonLeft, 1); err != nil {
		return fmt.Errorf("click MetLife password field: %w", err)
	}
	if err := passwordEl.Input(input.Password); err != nil {
		return fmt.Errorf("fill MetLife password: %w", err)
	}

	loginButton, err := page.Timeout(10 * time.Second).Element(`#signOnButtonSpan`)
	if err != nil {
		return fmt.Errorf("MetLife login button not found: %w", err)
	}
	if err := loginButton.Click(proto.InputMouseButtonLeft, 1); err != nil {
		return fmt.Errorf("click MetLife login: %w", err)
	}

	if waitForMFAChallenge(page, 10*time.Second) {
		if err := handleMFA(page, input); err != nil {
			return fmt.Errorf("MetLife MFA: %w", err)
		}
	}

	if err := waitForHome(page, 90*time.Second); err != nil {
		return err
	}
	if err := s.browser.SaveStorageState(s.storageStatePath); err != nil {
		return err
	}
	s.StartNetworkLogger()
	log.Printf("[MetLife] login complete: %s", currentURL(page))
	return nil
}

func ensureLoginFormVisible(page *rod.Page) error {
	if waitVisible(page, `#username`, 3*time.Second) {
		return nil
	}

	signInButton, err := page.Timeout(10 * time.Second).Element(`#mldc-react-button-secondary-white-signin`)
	if err != nil {
		return fmt.Errorf("MetLife pre-sign-in button not found and username field is still hidden: %w", err)
	}
	if err := signInButton.Click(proto.InputMouseButtonLeft, 1); err != nil {
		return fmt.Errorf("click MetLife pre-sign-in button: %w", err)
	}

	// Active session: Sign In redirects to /home instead of showing the login form.
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if isOnHome(page) {
			return nil
		}
		if waitVisible(page, `#username`, 500*time.Millisecond) {
			return nil
		}
	}
	return fmt.Errorf("MetLife username field not found after pre-sign-in click")
}

func (s *Session) StartNetworkLogger() {
	// Intentionally quiet during normal probe runs. Re-enable locally when
	// debugging portal XHR sequencing.
}

func waitForHome(page *rod.Page, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if isOnHome(page) {
			settleHomePage(page)
			return nil
		}
		time.Sleep(500 * time.Millisecond)
	}
	return fmt.Errorf("MetLife login did not reach home page: last URL=%s", currentURL(page))
}

// settleHomePage waits for the page to fully load and then pauses to allow
// Akamai's bot-validation JS to run and stamp the session cookies before we
// extract them for the API probe.
func settleHomePage(page *rod.Page) {
	_ = page.Timeout(15 * time.Second).WaitLoad()
	log.Printf("[MetLife] home page loaded, waiting for session cookies to settle")
	time.Sleep(8 * time.Second)
}

func isOnHome(page *rod.Page) bool {
	u := currentURL(page)
	return strings.Contains(u, "/home")
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

func waitVisible(page *rod.Page, selector string, timeout time.Duration) bool {
	if page == nil {
		return false
	}
	el, err := page.Timeout(timeout).Element(selector)
	if err != nil {
		return false
	}
	if err := el.WaitVisible(); err != nil {
		return false
	}
	return true
}

func shouldLogMetLifeEndpoint(url string) bool {
	return strings.Contains(url, "/md2/v1/metdental/eligibility/")
}

func summarizeMetLifeEndpoint(url string) string {
	switch {
	case strings.Contains(url, "/eligibility/overview"):
		return "/md2/v1/metdental/eligibility/overview"
	case strings.Contains(url, "/eligibility/planOverview"):
		return "/md2/v1/metdental/eligibility/planOverview"
	case strings.Contains(url, "/eligibility/procedureCategories"):
		return "/md2/v1/metdental/eligibility/procedureCategories"
	case strings.Contains(url, "/eligibility/procedureCodes"):
		return "/md2/v1/metdental/eligibility/procedureCodes"
	case strings.Contains(url, "/eligibility/providers"):
		return "/md2/v1/metdental/eligibility/providers"
	default:
		return url
	}
}

func waitForMFAChallenge(page *rod.Page, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if isOnHome(page) {
			return false
		}
		// Tile selection page appears before the OTP input; detect it first.
		if waitVisible(page, `div.tile-button.mfa1-input-field`, 500*time.Millisecond) {
			return true
		}
		if waitVisible(page, `input[autocomplete="one-time-code"], input[name*="code" i], input[id*="code" i], input[inputmode="numeric"]`, 500*time.Millisecond) {
			return true
		}
		time.Sleep(500 * time.Millisecond)
	}
	return false
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
	lastDash := false
	for _, r := range strings.TrimSpace(strings.ToLower(value)) {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			b.WriteRune(r)
			lastDash = false
		default:
			if !lastDash && b.Len() > 0 {
				b.WriteByte('-')
				lastDash = true
			}
		}
	}
	return strings.Trim(b.String(), "-")
}
