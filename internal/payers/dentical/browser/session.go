package browser

import (
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"
	"unicode"

	sharedbrowser "insurance-benefit-agent-go/internal/browser"
	"insurance-benefit-agent-go/internal/payers"

	"github.com/go-rod/rod"
	rodInput "github.com/go-rod/rod/lib/input"
	"github.com/go-rod/rod/lib/proto"
)

const loginURL = "https://provider-portal.apps.prd.cammis.medi-cal.ca.gov/login"
const transactionCenterURL = "https://provider-portal.apps.prd.cammis.medi-cal.ca.gov/transactionCenter"

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

func (s *Session) ProviderNPI() string {
	page := s.Page()
	if page == nil {
		return ""
	}
	res, err := page.Timeout(5 * time.Second).Eval(`() => {
		const all = [];
		const walk = (root) => {
			for (const node of root.querySelectorAll('*')) {
				all.push(node);
				if (node.shadowRoot) walk(node.shadowRoot);
			}
		};
		walk(document);
		for (const node of all) {
			const text = node.innerText || node.textContent || '';
			const match = text.match(/\bNPI\s*[:#]?\s*(\d{10})\b/i);
			if (match) return match[1];
		}
		return '';
	}`)
	if err != nil {
		log.Printf("[Denti-Cal] provider NPI discovery skipped: %v", err)
		return ""
	}
	return strings.TrimSpace(res.Value.String())
}

func (s *Session) Login(input payers.SessionInput) error {
	page := s.Page()
	if page == nil {
		return fmt.Errorf("browser session is not initialized")
	}
	if err := page.Navigate(loginURL); err != nil {
		return fmt.Errorf("navigate Denti-Cal login: %w", err)
	}
	waitSettle(page, 5*time.Second)

	if waitLoggedIn(page, 5*time.Second) == nil {
		if err := s.ensureTransactionCenter(page); err != nil {
			return err
		}
		return s.saveStorageState()
	}
	if err := fillCredentials(page, input); err != nil {
		return err
	}
	if err := submitLoginForm(page); err != nil {
		return err
	}
	if err := waitForULAOrLoggedIn(page, 45*time.Second); err != nil {
		return err
	}
	if strings.Contains(currentURL(page), "/ula") {
		if err := acceptAgreementAndContinue(page); err != nil {
			return err
		}
		if err := waitForTransactionCenter(page, 60*time.Second); err != nil {
			return err
		}
	} else if waitLoggedIn(page, 2*time.Second) == nil {
		if err := s.ensureTransactionCenter(page); err != nil {
			return err
		}
		return s.saveStorageState()
	}
	if err := waitLoggedIn(page, 60*time.Second); err != nil {
		return err
	}
	if err := s.ensureTransactionCenter(page); err != nil {
		return err
	}
	return s.saveStorageState()
}

func (s *Session) ensureTransactionCenter(page *rod.Page) error {
	if strings.Contains(currentURL(page), "/transactionCenter") {
		waitSettle(page, 5*time.Second)
		return nil
	}
	if err := page.Navigate(transactionCenterURL); err != nil {
		return fmt.Errorf("navigate Denti-Cal transaction center: %w", err)
	}
	return waitForTransactionCenter(page, 60*time.Second)
}

func (s *Session) saveStorageState() error {
	if err := s.browser.SaveStorageState(s.storageStatePath); err != nil {
		log.Printf("[Denti-Cal] storage state save skipped: %v", err)
	}
	log.Printf("[Denti-Cal] login complete: %s", currentURL(s.Page()))
	return nil
}

func fillCredentials(page *rod.Page, input payers.SessionInput) error {
	email, err := page.Timeout(20 * time.Second).Element(`input[placeholder="Email Address"], input[type="text"][maxlength="50"], input[type="email"]`)
	if err != nil {
		return fmt.Errorf("Denti-Cal email field not found: %w", err)
	}
	if err := email.SelectAllText(); err != nil {
		log.Printf("[Denti-Cal] email select-all skipped: %v", err)
	}
	if err := email.Input(input.Credential.Username); err != nil {
		return fmt.Errorf("fill Denti-Cal email: %w", err)
	}

	password, err := page.Timeout(20 * time.Second).Element(`input[placeholder="Password"], input[type="password"]`)
	if err != nil {
		return fmt.Errorf("Denti-Cal password field not found: %w", err)
	}
	if err := password.SelectAllText(); err != nil {
		log.Printf("[Denti-Cal] password select-all skipped: %v", err)
	}
	if err := password.Input(input.Password); err != nil {
		return fmt.Errorf("fill Denti-Cal password: %w", err)
	}
	time.Sleep(5 * time.Second)
	return nil
}

func submitLoginForm(page *rod.Page) error {
	if button, err := page.Timeout(10 * time.Second).Element(`button[type="submit"].Login-module__loginBtn__Z4uoD, button[type="submit"]`); err == nil {
		if visible, _ := button.Visible(); visible {
			if err := button.Click(proto.InputMouseButtonLeft, 1); err != nil {
				return fmt.Errorf("click Denti-Cal Log In button: %w", err)
			}
			waitSettle(page, 5*time.Second)
			return nil
		}
	}

	clicked, err := page.Timeout(10 * time.Second).Eval(`() => {
		const all = [];
		const walk = (root) => {
			for (const node of root.querySelectorAll('*')) {
				all.push(node);
				if (node.shadowRoot) walk(node.shadowRoot);
			}
		};
		walk(document);
		const candidates = all.filter((node) =>
			/^(BUTTON|A|DIV|SPAN|INPUT)$/i.test(node.tagName || '') &&
			/(Log In|Login|Sign In|Submit)/i.test(
				node.value || node.innerText || node.textContent || node.getAttribute?.('aria-label') || ''
			)
		);
		const target = candidates.find((node) => {
			const style = window.getComputedStyle(node);
			const rect = node.getBoundingClientRect();
			return style.visibility !== 'hidden' && style.display !== 'none' && rect.width > 0 && rect.height > 0;
		});
		if (!target) return false;
		const clickable = target.closest?.('button, a, [role="button"], input') || target;
		clickable.scrollIntoView({block: 'center', inline: 'center'});
		clickable.click();
		return true;
	}`)
	if err != nil {
		return fmt.Errorf("click Denti-Cal login submit: %w", err)
	}
	if clicked.Value.Bool() {
		waitSettle(page, 5*time.Second)
		return nil
	}

	password, _ := page.Timeout(2 * time.Second).Element(`input[placeholder="Password"], input[type="password"]`)
	if password != nil {
		_ = password.Focus()
	}
	if err := page.Keyboard.Press(rodInput.Enter); err != nil {
		return fmt.Errorf("press Enter on Denti-Cal login form: %w", err)
	}
	waitSettle(page, 5*time.Second)
	return nil
}

func waitForULAOrLoggedIn(page *rod.Page, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		u := currentURL(page)
		if strings.Contains(u, "/transactionCenter") || strings.Contains(u, "/ula") || profileLoggedIn(page) {
			return nil
		}
		time.Sleep(500 * time.Millisecond)
	}
	return fmt.Errorf("Denti-Cal login did not reach ULA or logged-in state: %s", loginPageDebug(page))
}

func waitForTransactionCenter(page *rod.Page, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if strings.Contains(currentURL(page), "/transactionCenter") {
			waitSettle(page, 5*time.Second)
			return nil
		}
		time.Sleep(500 * time.Millisecond)
	}
	return fmt.Errorf("Denti-Cal login did not reach transaction center: %s", loginPageDebug(page))
}

func acceptAgreementAndContinue(page *rod.Page) error {
	clicked, err := page.Timeout(10 * time.Second).Eval(`() => {
		const all = [];
		const walk = (root) => {
			for (const node of root.querySelectorAll('*')) {
				all.push(node);
				if (node.shadowRoot) walk(node.shadowRoot);
			}
		};
		walk(document);
		const text = (node) => (node.innerText || node.textContent || '').trim();
		const clickNode = (node) => {
			if (!node) return false;
			node.scrollIntoView({block: 'center', inline: 'center'});
			node.click();
			return true;
		};

		const label =
			all.find((node) => node.matches?.('label.legal[for^="checkboxid-"], label[for^="checkboxid-"]')) ||
			all.find((node) => /I confirm|read and agree|agree to the above/i.test(text(node)) &&
				/^(LABEL|DIV|SPAN|P|BUTTON)$/i.test(node.tagName || ''));
		if (label) {
			return clickNode(label.closest?.('label, button, [role="checkbox"], [role="button"]') || label);
		}
		const checkbox =
			all.find((node) => node.matches?.('input[id^="checkboxid-"], input[type="checkbox"], [role="checkbox"]'));
		if (checkbox) return clickNode(checkbox);
		return false;
	}`)
	if err != nil {
		return fmt.Errorf("click Denti-Cal agreement checkbox: %w", err)
	}
	if !clicked.Value.Bool() {
		return fmt.Errorf("Denti-Cal agreement checkbox not found: %s", loginPageDebug(page))
	}
	time.Sleep(5 * time.Second)

	nextClicked, err := page.Timeout(15 * time.Second).Eval(`() => {
		const all = [];
		const walk = (root) => {
			for (const node of root.querySelectorAll('*')) {
				all.push(node);
				if (node.shadowRoot) walk(node.shadowRoot);
			}
		};
		walk(document);
		const next = all.find((node) =>
			/^(BUTTON|A|DIV|SPAN)$/i.test(node.tagName || '') &&
			/^\s*Next\s*$/i.test(node.innerText || node.textContent || '')
		);
		if (!next) return false;
		const clickable = next.closest?.('button, a, [role="button"]') || next;
		clickable.scrollIntoView({block: 'center', inline: 'center'});
		clickable.click();
		return true;
	}`)
	if err != nil {
		return fmt.Errorf("click Denti-Cal Next: %w", err)
	}
	if !nextClicked.Value.Bool() {
		return fmt.Errorf("Denti-Cal Next button not found: %s", loginPageDebug(page))
	}
	waitSettle(page, 5*time.Second)
	return nil
}

func loginPageDebug(page *rod.Page) string {
	res, err := page.Timeout(3 * time.Second).Eval(`() => {
		const labels = Array.from(document.querySelectorAll('label')).map((node) =>
			(node.innerText || node.textContent || '').trim()
		).filter(Boolean).slice(0, 5);
		const inputs = Array.from(document.querySelectorAll('input')).map((node) => ({
			type: node.type || '',
			placeholder: node.placeholder || '',
			id: node.id || '',
			name: node.name || ''
		})).slice(0, 8);
		const buttons = Array.from(document.querySelectorAll('button')).map((node) =>
			(node.innerText || node.textContent || '').trim()
		).filter(Boolean).slice(0, 5);
		return JSON.stringify({url: location.href, labels, inputs, buttons});
	}`)
	if err != nil {
		return fmt.Sprintf("url=%s debug unavailable: %v", currentURL(page), err)
	}
	return res.Value.String()
}

func waitLoggedIn(page *rod.Page, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		if profileLoggedIn(page) {
			return nil
		}
		if strings.Contains(currentURL(page), "/login") && waitVisible(page, `input[placeholder="Email Address"], input[type="password"]`, 300*time.Millisecond) {
			lastErr = fmt.Errorf("still on login form")
		}
		time.Sleep(500 * time.Millisecond)
	}
	if lastErr != nil {
		return fmt.Errorf("Denti-Cal login did not complete: %w", lastErr)
	}
	return fmt.Errorf("Denti-Cal login did not complete: last URL=%s", currentURL(page))
}

func profileLoggedIn(page *rod.Page) bool {
	res, err := page.Timeout(5 * time.Second).Eval(`async () => {
		try {
			const response = await fetch('/graphql', {
				method: 'POST',
				headers: {'content-type': 'application/json'},
				credentials: 'include',
				body: JSON.stringify({
					query: ` + "`" + `{
						accountManagement {
							getUserProfile {
								loggedIn
								userId
								persona
								__typename
							}
							__typename
						}
					}` + "`" + `,
					variables: {}
				})
			});
			const payload = await response.json();
			return !!payload?.data?.accountManagement?.getUserProfile?.loggedIn;
		} catch (_) {
			return false;
		}
	}`)
	return err == nil && res.Value.Bool()
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
