package browser

// MFA handler for the newpayer portal.
//
// This file is called from session.go when waitForMFAChallenge() returns true.
// Supports three methods, chosen by input.Credential.MFAMethod:
//   "sms"   (default) — reads a 6-digit code from the office SMS inbox
//   "email"           — reads a code from the office email inbox
//   "none"            — portal has no MFA; this file is a no-op
//
// TODO(newpayer): For each method, replace the OTP-input selector and submit
// selector with the ones from this portal's MFA screen.

import (
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/proto"

	"insurance-benefit-agent-go/internal/mfa"
	"insurance-benefit-agent-go/internal/payers"
)

// TODO(newpayer): adjust this pattern to match the OTP format this portal sends.
// Most dental portals use exactly 6 digits. Some use 8. Add a letter prefix if needed.
var otpPattern = regexp.MustCompile(`\b(\d{6})\b`)

func handleMFA(page *rod.Page, input payers.SessionInput) error {
	method := strings.ToLower(strings.TrimSpace(input.Credential.MFAMethod))
	if method == "" {
		method = "sms"
	}

	switch {
	case method == "none":
		return nil
	case strings.Contains(method, "email"):
		return completeEmailMFA(page, input)
	default:
		return completeSMSMFA(page, input)
	}
}

// completeSMSMFA waits for a 6-digit code to arrive via SMS and submits it.
func completeSMSMFA(page *rod.Page, input payers.SessionInput) error {
	// TODO(newpayer): if the portal shows a "send code via text" button before
	// the OTP input appears, click it here first:
	//   btn, err := page.Timeout(5*time.Second).Element(`#send-sms-btn`)
	//   if err == nil { _ = btn.Click(proto.InputMouseButtonLeft, 1) }

	codeSentAt := time.Now()

	smsCfg, err := mfa.SMSConfigFromScraperConfig(input.ScraperConfig, input.RequestedOfficeKey)
	if err != nil {
		return fmt.Errorf("NewPayer SMS config: %w", err)
	}
	code, err := mfa.GetSmsCodeByPattern(*smsCfg, codeSentAt, otpPattern)
	if err != nil {
		return fmt.Errorf("get NewPayer SMS code: %w", err)
	}
	return submitOTPCode(page, code)
}

// completeEmailMFA waits for a code to arrive in the office email inbox and submits it.
func completeEmailMFA(page *rod.Page, input payers.SessionInput) error {
	if input.EmailMFA == nil {
		return fmt.Errorf("NewPayer email MFA requested but EmailMFA config is missing from SessionInput")
	}

	// TODO(newpayer): if the portal shows a "send code via email" button, click it here.

	codeRequestedAt := time.Now()
	code, err := mfa.GetEmailCode(*input.EmailMFA, codeRequestedAt)
	if err != nil {
		return fmt.Errorf("get NewPayer email code: %w", err)
	}
	return submitOTPCode(page, code)
}

// submitOTPCode types the OTP into the code field and clicks the confirm button.
func submitOTPCode(page *rod.Page, code string) error {
	// TODO(newpayer): replace the selector list with this portal's OTP input.
	// The list below covers the most common patterns across dental portals.
	otpField, err := page.Timeout(20 * time.Second).Element(
		`input[autocomplete="one-time-code"], input[name*="code" i], input[id*="otp" i], input[inputmode="numeric"]`,
	)
	if err != nil {
		return fmt.Errorf("NewPayer OTP input not found: %w", err)
	}
	if err := otpField.Input(code); err != nil {
		return fmt.Errorf("fill NewPayer OTP: %w", err)
	}

	// TODO(newpayer): replace `#verify-btn` with the confirm/submit button selector.
	submitBtn, err := page.Timeout(10 * time.Second).Element(`#verify-btn`)
	if err != nil {
		// Fallback: look for any visible submit/verify button.
		submitBtn, err = page.Timeout(5*time.Second).ElementR(
			`button[type="submit"], input[type="submit"]`,
			`(?i)verify|confirm|continue|submit`,
		)
	}
	if err != nil {
		return fmt.Errorf("NewPayer MFA submit button not found: %w", err)
	}
	if err := submitBtn.Click(proto.InputMouseButtonLeft, 1); err != nil {
		return fmt.Errorf("click NewPayer MFA submit: %w", err)
	}
	return nil
}
