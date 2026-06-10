package browser

import (
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"
	"unicode"

	sharedbrowser "insurance-benefit-agent-go/internal/browser"
	"insurance-benefit-agent-go/internal/mfa"
	"insurance-benefit-agent-go/internal/payers"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/proto"
)

const (
	loginURL          = "https://login.guardianlife.com/"
	homeURL           = "https://www.guardiananytime.com/gaprovider/home"
	benefitsSearchURL = "https://www.guardiananytime.com/gaprovider/benefits-search"
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

func (s *Session) Login(input payers.SessionInput) error {
	page := s.Page()
	if page == nil {
		return fmt.Errorf("browser session is not initialized")
	}
	if err := page.Navigate(loginURL); err != nil {
		return fmt.Errorf("navigate Guardian login: %w", err)
	}
	waitSettle(page, 2*time.Second)
	if waitForGuardianPortal(page, 8*time.Second) == nil {
		return s.afterLogin(page)
	}
	if err := fillCredentials(page, input); err != nil {
		return err
	}
	state := waitPostCredentialState(page, 30*time.Second)
	if state != "portal" {
		if err := handleSMSMFA(page, input, state); err != nil {
			return err
		}
	}
	if err := waitForGuardianPortal(page, 90*time.Second); err != nil {
		return err
	}
	return s.afterLogin(page)
}

func (s *Session) afterLogin(page *rod.Page) error {
	if err := page.Navigate(benefitsSearchURL); err != nil {
		return fmt.Errorf("navigate Guardian benefits search: %w", err)
	}
	waitSettle(page, 3*time.Second)
	if err := waitBenefitsSearch(page, 45*time.Second); err != nil {
		return err
	}
	if err := s.browser.SaveStorageState(s.storageStatePath); err != nil {
		log.Printf("[Guardian] storage state save skipped: %v", err)
	}
	log.Printf("[Guardian] login complete: %s", currentURL(page))
	return nil
}

func fillCredentials(page *rod.Page, input payers.SessionInput) error {
	username, err := page.Timeout(20 * time.Second).Element(`input[name="identifier"], input[autocomplete="username"]`)
	if err != nil {
		return fmt.Errorf("Guardian username field not found: %w", err)
	}
	if err := username.SelectAllText(); err != nil {
		return fmt.Errorf("clear Guardian username: %w", err)
	}
	if err := username.Input(input.Credential.Username); err != nil {
		return fmt.Errorf("fill Guardian username: %w", err)
	}
	if password, err := page.Timeout(5 * time.Second).Element(`input[name="credentials.passcode"], input[type="password"]`); err == nil {
		if err := password.SelectAllText(); err != nil {
			return fmt.Errorf("clear Guardian password: %w", err)
		}
		if err := password.Input(input.Password); err != nil {
			return fmt.Errorf("fill Guardian password: %w", err)
		}
	} else if next, err := page.Timeout(5 * time.Second).Element(`input[type="submit"], button[type="submit"]`); err == nil {
		_ = next.Click(proto.InputMouseButtonLeft, 1)
		waitSettle(page, time.Second)
		password, err := page.Timeout(20 * time.Second).Element(`input[name="credentials.passcode"], input[type="password"]`)
		if err != nil {
			return fmt.Errorf("Guardian password field not found: %w", err)
		}
		if err := password.SelectAllText(); err != nil {
			return fmt.Errorf("clear Guardian password: %w", err)
		}
		if err := password.Input(input.Password); err != nil {
			return fmt.Errorf("fill Guardian password: %w", err)
		}
	}
	if remember, err := page.Timeout(3 * time.Second).Element(`input[name="rememberMe"]`); err == nil {
		_ = remember.Click(proto.InputMouseButtonLeft, 1)
	}
	submit, err := page.Timeout(15 * time.Second).Element(`input[type="submit"][value="Log in"], input[type="submit"], button[type="submit"]`)
	if err != nil {
		return fmt.Errorf("Guardian login submit not found: %w", err)
	}
	if err := submit.Click(proto.InputMouseButtonLeft, 1); err != nil {
		return fmt.Errorf("click Guardian login: %w", err)
	}
	waitSettle(page, 2*time.Second)
	return nil
}

func waitPostCredentialState(page *rod.Page, timeout time.Duration) string {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if isGuardianPortal(page) {
			return "portal"
		}
		if waitVisible(page, `[data-se="phone_number"] a`, 300*time.Millisecond) || guardianSMSFactorVisible(page) {
			return "mfa_select"
		}
		if waitVisible(page, `form.phone-authenticator-challenge`, 300*time.Millisecond) {
			return "mfa_sms"
		}
		time.Sleep(300 * time.Millisecond)
	}
	return "timeout"
}

func handleSMSMFA(page *rod.Page, input payers.SessionInput, state string) error {
	if state == "mfa_select" {
		clicked, err := clickGuardianSMSFactor(page, 15*time.Second)
		if err != nil {
			return err
		}
		if !clicked {
			return fmt.Errorf("Guardian SMS/Text factor not found")
		}
		waitSettle(page, time.Second)
	}
	if button, err := page.Timeout(15 * time.Second).Element(`input[type="submit"][value*="Receive a code"], input[type="submit"], button[type="submit"]`); err == nil {
		codeRequestedAt := time.Now()
		if err := button.Click(proto.InputMouseButtonLeft, 1); err != nil {
			return fmt.Errorf("request Guardian SMS code: %w", err)
		}
		smsCfg, err := mfa.SMSConfigFromScraperConfig(input.ScraperConfig, input.RequestedOfficeKey)
		if err != nil {
			return fmt.Errorf("Guardian SMS config: %w", err)
		}
		smsCfg.TimeoutMS = maxInt(smsCfg.TimeoutMS, 150000)
		code, err := mfa.GetSmsCode(*smsCfg, codeRequestedAt)
		if err != nil {
			return fmt.Errorf("get Guardian SMS code: %w", err)
		}
		return submitMFACode(page, code)
	}
	return fmt.Errorf("Guardian SMS MFA request button not found")
}

func guardianSMSFactorVisible(page *rod.Page) bool {
	res, err := page.Eval(`() => {
		const rows = Array.from(document.querySelectorAll('.authenticator-row'));
		return rows.some(row => /Text\/Voice|Text|SMS/i.test(row.innerText || row.textContent || ''));
	}`)
	return err == nil && res.Value.Bool()
}

func clickGuardianSMSFactor(page *rod.Page, timeout time.Duration) (bool, error) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		res, err := page.Eval(`() => {
			const phone = document.querySelector('[data-se="phone_number"] a');
			if (phone) {
				phone.scrollIntoView({block: 'center', inline: 'center'});
				phone.click();
				return true;
			}
			const rows = Array.from(document.querySelectorAll('.authenticator-row'));
			for (const row of rows) {
				const text = row.innerText || row.textContent || '';
				if (!/Text\/Voice|Text|SMS/i.test(text)) continue;
				const link = row.querySelector('a.select-factor, a[data-se="button"], a, button');
				if (!link) continue;
				link.scrollIntoView({block: 'center', inline: 'center'});
				link.click();
				return true;
			}
			return false;
		}`)
		if err != nil {
			return false, fmt.Errorf("select Guardian SMS MFA: %w", err)
		}
		if res.Value.Bool() {
			log.Printf("[Guardian] selected Text/Voice MFA factor")
			return true, nil
		}
		time.Sleep(300 * time.Millisecond)
	}
	return false, nil
}

func submitMFACode(page *rod.Page, code string) error {
	input, err := page.Timeout(45 * time.Second).Element(`input[name="credentials.passcode"], input[autocomplete="one-time-code"], input[type="text"], input[type="tel"]`)
	if err != nil {
		return fmt.Errorf("Guardian MFA code field not found: %w", err)
	}
	if err := input.Input(code); err != nil {
		return fmt.Errorf("fill Guardian MFA code: %w", err)
	}
	submit, err := page.Timeout(15 * time.Second).Element(`input[type="submit"], button[type="submit"]`)
	if err != nil {
		return fmt.Errorf("Guardian MFA submit not found: %w", err)
	}
	if err := submit.Click(proto.InputMouseButtonLeft, 1); err != nil {
		return fmt.Errorf("submit Guardian MFA: %w", err)
	}
	waitSettle(page, 3*time.Second)
	return nil
}

func waitForGuardianPortal(page *rod.Page, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if isGuardianPortal(page) {
			waitSettle(page, 2*time.Second)
			return nil
		}
		time.Sleep(500 * time.Millisecond)
	}
	return fmt.Errorf("Guardian login did not reach portal: %s", currentURL(page))
}

func waitBenefitsSearch(page *rod.Page, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if waitVisible(page, `#member-id-input-0, #member-search-submit-btn`, 500*time.Millisecond) || strings.Contains(currentURL(page), "/benefits-search") {
			return nil
		}
		time.Sleep(500 * time.Millisecond)
	}
	return fmt.Errorf("Guardian benefits search did not load: %s", currentURL(page))
}

func isGuardianPortal(page *rod.Page) bool {
	u := currentURL(page)
	return strings.Contains(u, "guardiananytime.com/gaprovider")
}

func waitSettle(page *rod.Page, d time.Duration) {
	_ = page.Timeout(10 * time.Second).WaitLoad()
	time.Sleep(d)
}

func waitVisible(page *rod.Page, selector string, timeout time.Duration) bool {
	el, err := page.Timeout(timeout).Element(selector)
	if err != nil {
		return false
	}
	visible, err := el.Visible()
	return err == nil && visible
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

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
