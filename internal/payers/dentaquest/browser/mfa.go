package browser

import (
	"fmt"
	"log"
	"time"

	"insurance-benefit-agent-go/internal/mfa"
	"insurance-benefit-agent-go/internal/payers"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/proto"
	rodInput "github.com/go-rod/rod/lib/input"
)

func handleEmailMFA(page *rod.Page, input payers.SessionInput) error {
	codeRequestedAt := time.Now()
	if err := waitForCodeInput(page); err != nil {
		return err
	}
	if input.EmailMFA == nil {
		return fmt.Errorf("DentaQuest email MFA requested but email config is missing")
	}

	code, err := mfa.GetEmailCode(*input.EmailMFA, codeRequestedAt)
	if err != nil {
		return err
	}

	return submitVerificationCode(page, code)
}

func isEmailMFAPromptVisible(page *rod.Page) bool {
	selectors := []string{
		`input[data-testid="passcode"]`,
		`input[placeholder="Enter your verification code"]`,
		`input[autocomplete="one-time-code"]`,
		`input[name*="code" i]`,
		`input[id*="code" i]`,
		`input[inputmode="numeric"]`,
	}
	for _, sel := range selectors {
		if isVisible(page, sel) {
			return true
		}
	}
	return false
}

func waitForCodeInput(page *rod.Page) error {
	_, err := verificationCodeInput(page, 30*time.Second)
	if err != nil {
		return fmt.Errorf("wait for DentaQuest MFA code input: %w", err)
	}
	return nil
}

func submitVerificationCode(page *rod.Page, code string) error {
	codeEl, err := verificationCodeInput(page, 10*time.Second)
	if err != nil {
		return fmt.Errorf("wait for MFA verification input: %w", err)
	}
	if err := codeEl.Input(code); err != nil {
		return fmt.Errorf("fill MFA verification code: %w", err)
	}

	// Some portals enable the submit button only after the full code is typed.
	time.Sleep(500 * time.Millisecond)

	// Try an explicit verify button first (with a short wait for it to appear).
	// Match by aria-label attribute — DentaQuest wraps the button text in a <span>
	// so text-content regex matching misses it.
	const verifySel = `button[aria-label*="verify" i], button[aria-label*="continue" i], ` +
		`button[aria-label*="submit" i], input[type="submit"]`
	verifyEl, err := page.Timeout(3 * time.Second).Element(verifySel)
	if err == nil {
		if clickErr := verifyEl.Click(proto.InputMouseButtonLeft, 1); clickErr != nil {
			log.Printf("[DentaQuest] MFA verify button click failed: %v; trying Enter", clickErr)
		} else {
			return waitForDashboard(page, 60*time.Second)
		}
	} else {
		log.Printf("[DentaQuest] MFA verify button not found; pressing Enter")
	}

	// Fall back to Enter on the code field — re-query it in case the reference is stale.
	if fresh, err := verificationCodeInput(page, 3*time.Second); err == nil {
		codeEl = fresh
	}
	_ = codeEl.Focus()
	if err := page.Keyboard.Press(rodInput.Enter); err != nil {
		return fmt.Errorf("submit MFA code with Enter: %w", err)
	}

	return waitForDashboard(page, 60*time.Second)
}

// verificationCodeInput returns the first visible MFA code input, trying each
// selector in order and polling until timeout.
func verificationCodeInput(page *rod.Page, timeout time.Duration) (*rod.Element, error) {
	selectors := []string{
		`input[data-testid="passcode"]`,
		`input[placeholder="Enter your verification code"]`,
		`input[autocomplete="one-time-code"]`,
		`input[inputmode="numeric"]`,
		`input[name*="code" i]`,
		`input[id*="code" i]`,
	}

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		for _, sel := range selectors {
			el, err := page.Element(sel)
			if err != nil {
				continue
			}
			if visible, err := el.Visible(); err == nil && visible {
				return el, nil
			}
		}
		time.Sleep(200 * time.Millisecond)
	}

	// Last resort: any visible text input on the page.
	return page.Timeout(2 * time.Second).Element(`input[type="text"], input:not([type])`)
}

func waitForDashboard(page *rod.Page, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if hasAuthenticatedPortalContent(page) {
			_ = page.Timeout(3 * time.Second).WaitLoad()
			return nil
		}

		if el, err := page.ElementR(`button`, `(?i)continue|next|submit`); err == nil {
			if visible, _ := el.Visible(); visible {
				_ = el.Click(proto.InputMouseButtonLeft, 1)
			}
		}

		time.Sleep(time.Second)
	}

	return fmt.Errorf("DentaQuest email MFA did not reach the dashboard in time")
}
