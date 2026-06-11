package browser

import (
	"fmt"
	"regexp"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/proto"

	"insurance-benefit-agent-go/internal/mfa"
	"insurance-benefit-agent-go/internal/payers"
)

// Delta Dental sends a 6-digit OTP via SMS.
var ddSMSPattern = regexp.MustCompile(`\b(\d{6})\b`)

// handleMFA handles the new portal MFA screen:
//
//	<input aria-label="Confirmation Code" class="form-control">
//	<button class="btn btn-primary ml-0 w-100">Confirm</button>
func handleMFA(page *rod.Page, input payers.SessionInput) error {
	method := input.Credential.MFAMethod
	if method == "" {
		method = "sms"
	}

	if method == "email" {
		return completeEmailMFA(page, input)
	}
	return completeSMSMFA(page, input)
}

func completeSMSMFA(page *rod.Page, input payers.SessionInput) error {
	codeSentAt := time.Now()

	smsCfg, err := mfa.SMSConfigFromScraperConfig(input.ScraperConfig, input.RequestedOfficeKey)
	if err != nil {
		return fmt.Errorf("Delta Dental SMS config: %w", err)
	}
	code, err := mfa.GetSmsCodeByPattern(*smsCfg, codeSentAt, ddSMSPattern)
	if err != nil {
		return fmt.Errorf("get Delta Dental SMS code: %w", err)
	}
	return submitConfirmationCode(page, code)
}

func completeEmailMFA(page *rod.Page, input payers.SessionInput) error {
	if input.EmailMFA == nil {
		return fmt.Errorf("Delta Dental email MFA requested but EmailMFA config is missing")
	}
	codeRequestedAt := time.Now()
	code, err := mfa.GetEmailCode(*input.EmailMFA, codeRequestedAt)
	if err != nil {
		return fmt.Errorf("get Delta Dental email code: %w", err)
	}
	return submitConfirmationCode(page, code)
}

// submitConfirmationCode fills the Confirmation Code input and clicks Confirm.
func submitConfirmationCode(page *rod.Page, code string) error {
	// Wait for the code input to be visible.
	codeEl, err := page.Timeout(20 * time.Second).Element(`input[aria-label="Confirmation Code"]`)
	if err != nil {
		return fmt.Errorf("Delta Dental Confirmation Code input not found: %w", err)
	}
	if err := codeEl.Input(code); err != nil {
		return fmt.Errorf("fill Delta Dental Confirmation Code: %w", err)
	}

	// Click the Confirm button.
	confirmBtn, err := page.Timeout(10 * time.Second).ElementR(`button`, `(?i)confirm`)
	if err != nil {
		// Fallback: any primary submit button.
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
