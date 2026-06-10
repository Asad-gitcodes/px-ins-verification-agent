package browser

import (
	"fmt"
	"time"

	"insurance-benefit-agent-go/internal/mfa"
	"insurance-benefit-agent-go/internal/payers"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/proto"
)

// handleMFA dispatches to the correct MFA flow based on the post-login state
// and the credential's MFA method. Mirrors handleMfa() in loginFlow.js.
func handleMFA(page *rod.Page, state string, input payers.SessionInput) error {
	method := input.Credential.MFAMethod

	// Step 1: if the authenticator selection screen appeared, pick one.
	// Both "mfa_select" and "mfa_sms" indicate the selection screen —
	// the difference is only which element was spotted first.
	if state == "mfa_select" || state == "mfa_sms" {
		selected, err := selectAuthenticator(page, method)
		if err != nil {
			return err
		}
		method = selected
		// Give the chosen authenticator screen time to load.
		time.Sleep(500 * time.Millisecond)
	}

	if state == "mfa_sms" || method == "sms" {
		if !waitVisible(page, `input[type="submit"][value="Receive a code via SMS"]`, 10*time.Second) {
			return fmt.Errorf("SMS code button not visible")
		}
		el, err := page.Element(`input[type="submit"][value="Receive a code via SMS"]`)
		if err != nil {
			return fmt.Errorf("fetch SMS code button: %w", err)
		}
		if err := el.Click(proto.InputMouseButtonLeft, 1); err != nil {
			return fmt.Errorf("click SMS code button: %w", err)
		}
		if err := completeSMSMFA(page, input); err != nil {
			return err
		}
		return nil
	}
	// // Step 2: SMS confirmation — Okta shows "Receive a code via SMS" button.
	// if el, err := page.Timeout(5*time.Second).ElementR(`button`, `Receive a code via SMS`); err == nil {
	// 	if visible, _ := el.Visible(); visible {
	// 		if err := completeSMSMFA(page, el, input); err != nil {
	// 			return err
	// 		}
	// 		return nil
	// 	}
	// }

	// Step 3: email MFA.
	if method == "email" || input.EmailMFA != nil {
		return completeEmailMFA(page, input)
	}

	return fmt.Errorf("no supported MFA flow could be determined (method=%q state=%q)", method, state)
}

// selectAuthenticator picks phone or email from the Okta authenticator list.
// Mirrors selectAuthenticator() in loginFlow.js.
func selectAuthenticator(page *rod.Page, preferredMethod string) (string, error) {
	phoneVisible := waitVisible(page, `[data-se="phone_number"] a`, 2*time.Second)
	emailVisible := waitVisible(page, `[data-se="okta_email"] a`, 2*time.Second)

	if preferredMethod == "email" && emailVisible {
		el, _ := page.Element(`[data-se="okta_email"] a`)
		_ = el.Click(proto.InputMouseButtonLeft, 1)
		return "email", nil
	}
	if preferredMethod == "sms" && phoneVisible {
		el, _ := page.Element(`[data-se="phone_number"] a`)
		_ = el.Click(proto.InputMouseButtonLeft, 1)
		return "sms", nil
	}
	// Default preference: phone over email.
	if phoneVisible {
		el, _ := page.Element(`[data-se="phone_number"] a`)
		_ = el.Click(proto.InputMouseButtonLeft, 1)
		return "sms", nil
	}
	if emailVisible {
		el, _ := page.Element(`[data-se="okta_email"] a`)
		_ = el.Click(proto.InputMouseButtonLeft, 1)
		return "email", nil
	}

	return "", fmt.Errorf("no supported MFA authenticator was available on selection screen")
}

// completeSMSMFA clicks the SMS button, fetches the code, and submits it.
func completeSMSMFA(page *rod.Page, input payers.SessionInput) error {
	codeSentAt := time.Now()

	smsCfg, err := mfa.SMSConfigFromScraperConfig(input.ScraperConfig, input.RequestedOfficeKey)
	if err != nil {
		return fmt.Errorf("SMS config: %w", err)
	}
	code, err := mfa.GetSmsCode(*smsCfg, codeSentAt)
	if err != nil {
		return fmt.Errorf("get SMS MFA code: %w", err)
	}
	return submitMFACode(page, code)
}

// completeEmailMFA triggers email code delivery, retrieves the code, and submits it.
// Mirrors completeEmailMfa() + triggerEmailCodeDelivery() in loginFlow.js.
func completeEmailMFA(page *rod.Page, input payers.SessionInput) error {
	if input.EmailMFA == nil {
		return fmt.Errorf("Delta Dental email MFA requested but email config is missing")
	}

	codeRequestedAt := time.Now()

	// Some Okta flows require clicking a "Send me the code" button first;
	// others auto-send. Try common button patterns, skip if none are found.
	sendPatterns := []string{
		`Send me the code`,
		`Send Code`,
		`Email me`,
		`^Send$`,
	}
	for _, pattern := range sendPatterns {
		if el, err := page.Timeout(2*time.Second).ElementR(`button`, pattern); err == nil {
			if visible, _ := el.Visible(); visible {
				_ = el.Click(proto.InputMouseButtonLeft, 1)
				break
			}
		}
	}

	code, err := mfa.GetEmailCode(*input.EmailMFA, codeRequestedAt)
	if err != nil {
		return fmt.Errorf("get email MFA code: %w", err)
	}

	return submitMFACode(page, code)
}

// submitMFACode fills the code input and clicks Verify.
// Mirrors submitMfaCode() in loginFlow.js.
func submitMFACode(page *rod.Page, code string) error {
	time.Sleep(20 * time.Second)
	if !waitVisible(page, `input[name="credentials.passcode"]`, 10*time.Second) {
		return fmt.Errorf("passcode input not visible")
	}
	el, err := page.Element(`input[name="credentials.passcode"]`)
	if err != nil {
		return fmt.Errorf("fetch passcode input: %w", err)
	}
	if err := el.Input(code); err != nil {
		return fmt.Errorf("fill passcode: %w", err)
	}

	// codeEl, err := mfaCodeInput(page, 10*time.Second)
	// if err != nil {
	// 	return fmt.Errorf("MFA code input not found: %w", err)
	// }
	// if err := codeEl.Input(code); err != nil {
	// 	return fmt.Errorf("fill MFA code: %w", err)
	// }
	time.Sleep(300 * time.Millisecond)

	verifyBtn, err := page.Timeout(5 * time.Second).Element(`input[type="submit"][value="Verify"]`)
	if err != nil {
		return fmt.Errorf("Verify button not found after code entry: %w", err)
	}
	if err := verifyBtn.Click(proto.InputMouseButtonLeft, 1); err != nil {
		return fmt.Errorf("click Verify: %w", err)
	}
	return nil
}

// mfaCodeInput returns the visible one-time code input, polling until timeout.
func mfaCodeInput(page *rod.Page, timeout time.Duration) (*rod.Element, error) {
	selectors := []string{
		`input[autocomplete="one-time-code"]`,
		`input[inputmode="numeric"]`,
		`input[name*="code" i]`,
		`input[id*="code" i]`,
		`input[type="text"]`,
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
	return nil, fmt.Errorf("no visible MFA code input found within timeout")
}
