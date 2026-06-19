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

var ddOTPPattern = regexp.MustCompile(`\b(\d{6})\b`)

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

func completeSMSMFA(page *rod.Page, input payers.SessionInput) error {
	// DB stores Pacific time; use 3-hour lookback to handle timezone offset between
	// Mac (Central) and DB server (Pacific).
	codeSentAt := time.Now().Add(-3 * time.Hour)

	smsCfg, err := mfa.SMSConfigFromScraperConfig(input.ScraperConfig, input.RequestedOfficeKey)
	if err != nil {
		return fmt.Errorf("Delta Dental WA SMS config: %w", err)
	}
	code, err := mfa.GetSmsCodeByPattern(*smsCfg, codeSentAt, ddOTPPattern)
	if err != nil {
		return fmt.Errorf("get Delta Dental WA SMS OTP: %w", err)
	}
	return submitConfirmationCode(page, code)
}

func completeEmailMFA(page *rod.Page, input payers.SessionInput) error {
	if input.EmailMFA == nil {
		return fmt.Errorf("Delta Dental WA email MFA requested but EmailMFA config is missing")
	}
	codeRequestedAt := time.Now()
	code, err := mfa.GetEmailCode(*input.EmailMFA, codeRequestedAt)
	if err != nil {
		return fmt.Errorf("get Delta Dental WA email OTP: %w", err)
	}
	return submitConfirmationCode(page, code)
}

func submitConfirmationCode(page *rod.Page, code string) error {
	codeEl, err := page.Timeout(20 * time.Second).Element(`input[aria-label="Confirmation Code"]`)
	if err != nil {
		return fmt.Errorf("Delta Dental WA Confirmation Code input not found: %w", err)
	}
	if err := codeEl.Input(code); err != nil {
		return fmt.Errorf("fill Delta Dental WA Confirmation Code: %w", err)
	}
	time.Sleep(200 * time.Millisecond)

	confirmBtn, err := page.Timeout(10 * time.Second).ElementR(`button`, `(?i)^confirm$`)
	if err != nil {
		confirmBtn, err = page.Timeout(5*time.Second).Element(`button.btn-primary`)
	}
	if err != nil {
		return fmt.Errorf("Delta Dental WA Confirm button not found: %w", err)
	}
	if err := confirmBtn.Click(proto.InputMouseButtonLeft, 1); err != nil {
		return fmt.Errorf("click Delta Dental WA Confirm: %w", err)
	}
	return nil
}
