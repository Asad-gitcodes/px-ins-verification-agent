package browser

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

// Delta Dental sends a 6-digit OTP via SMS or email.
var ddOTPPattern = regexp.MustCompile(`\b(\d{6})\b`)

// handleMFA handles the MFA screen that appears after SIGN IN:
//
//	<input aria-label="Confirmation Code" class="form-control">
//	<button class="btn btn-primary ml-0 w-100">Confirm</button>
//
// Method is chosen by input.Credential.MFAMethod ("sms" | "email").
// Defaults to "sms" if unset.
func handleMFA(page *rod.Page, input payers.SessionInput) error {
	method := strings.ToLower(strings.TrimSpace(input.Credential.MFAMethod))
	if method == "" {
		method = "sms"
	}

	switch method {
	case "email":
		return completeEmailMFA(page, input)
	default:
		return completeSMSMFA(page, input)
	}
}

// completeSMSMFA reads the 6-digit OTP from the office SMS inbox and submits it.
func completeSMSMFA(page *rod.Page, input payers.SessionInput) error {
	// DB stores UTC timestamps; compare against UTC now with a 2-minute lookback
	// to absorb any clock skew between the Mac and the DB server.
	codeSentAt := time.Now().UTC().Add(-2 * time.Minute)

	smsCfg, err := mfa.SMSConfigFromScraperConfig(input.ScraperConfig, input.RequestedOfficeKey)
	if err != nil {
		return fmt.Errorf("Delta Dental SMS config: %w", err)
	}
	code, err := mfa.GetSmsCodeByPattern(*smsCfg, codeSentAt, ddOTPPattern)
	if err != nil {
		return fmt.Errorf("get Delta Dental SMS OTP: %w", err)
	}
	return submitConfirmationCode(page, code)
}

// completeEmailMFA reads the OTP from the office email inbox and submits it.
func completeEmailMFA(page *rod.Page, input payers.SessionInput) error {
	if input.EmailMFA == nil {
		return fmt.Errorf("Delta Dental email MFA requested but EmailMFA config is missing")
	}
	codeRequestedAt := time.Now()
	code, err := mfa.GetEmailCode(*input.EmailMFA, codeRequestedAt)
	if err != nil {
		return fmt.Errorf("get Delta Dental email OTP: %w", err)
	}
	return submitConfirmationCode(page, code)
}

// submitConfirmationCode fills the Confirmation Code input and clicks Confirm.
// Selectors confirmed from portal HTML:
//
//	<input aria-label="Confirmation Code" class="form-control">
//	<button class="btn btn-primary ml-0 w-100">Confirm</button>
func submitConfirmationCode(page *rod.Page, code string) error {
	codeEl, err := page.Timeout(20 * time.Second).Element(`input[aria-label="Confirmation Code"]`)
	if err != nil {
		return fmt.Errorf("Delta Dental Confirmation Code input not found: %w", err)
	}
	if err := codeEl.Input(code); err != nil {
		return fmt.Errorf("fill Delta Dental Confirmation Code: %w", err)
	}
	time.Sleep(200 * time.Millisecond)

	// Try exact text match first, then class-based fallback.
	confirmBtn, err := page.Timeout(10 * time.Second).ElementR(`button`, `(?i)^confirm$`)
	if err != nil {
		confirmBtn, err = page.Timeout(5*time.Second).Element(`button.btn-primary`)
	}
	if err != nil {
		return fmt.Errorf("Delta Dental Confirm button not found: %w", err)
	}
	if err := confirmBtn.Click(proto.InputMouseButtonLeft, 1); err != nil {
		return fmt.Errorf("click Delta Dental Confirm: %w", err)
	}
	return nil
}
