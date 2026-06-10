package browser

import (
	"fmt"
	"log"
	"regexp"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/proto"

	"insurance-benefit-agent-go/internal/mfa"
	"insurance-benefit-agent-go/internal/payers"
)

// uhcOTPPattern matches "Your One Healthcare ID access code is: 1234567"
// UHC sends 7-digit codes, so we capture any digit run after the colon.
var uhcOTPPattern = regexp.MustCompile(`access code is:\s*(\d+)`)

// handleMFA detects whether UHC Optum SSO is showing an MFA screen and
// completes the SMS OTP flow if so. Returns nil when not on an MFA screen
// or after successful submission.
func handleMFA(page *rod.Page, input payers.SessionInput) error {
	// Check for the text-message option button (#textMsg).
	if !waitVisible(page, `#textMsg`, 8*time.Second) {
		// No MFA screen detected.
		return nil
	}

	log.Printf("[UHCDental] MFA screen detected, requesting SMS code")

	el, err := page.Element(`#textMsg`)
	if err != nil {
		return fmt.Errorf("find #textMsg: %w", err)
	}
	if err := el.Click(proto.InputMouseButtonLeft, 1); err != nil {
		return fmt.Errorf("click #textMsg: %w", err)
	}

	codeSentAt := time.Now()

	smsCfg, err := mfa.SMSConfigFromScraperConfig(input.ScraperConfig, input.RequestedOfficeKey)
	if err != nil {
		return fmt.Errorf("UHC SMS config: %w", err)
	}

	// Wait up to 2 minutes for the SMS — UHC delivery can be slow.
	smsCfg.TimeoutMS = 120_000

	code, err := mfa.GetSmsCodeByPattern(*smsCfg, codeSentAt, uhcOTPPattern)
	if err != nil {
		return fmt.Errorf("get UHC SMS OTP: %w", err)
	}

	if !waitVisible(page, `#otpBox`, 15*time.Second) {
		return fmt.Errorf("OTP input #otpBox not visible")
	}
	otpEl, err := page.Element(`#otpBox`)
	if err != nil {
		return fmt.Errorf("find #otpBox: %w", err)
	}
	if err := otpEl.Input(code); err != nil {
		return fmt.Errorf("fill OTP: %w", err)
	}

	// Check "Remember this device" so future logins skip MFA.
	if waitVisible(page, `[data-cy="data-checkbox-rememberDevice"]`, 3*time.Second) {
		if rememberEl, err := page.Element(`[data-cy="data-checkbox-rememberDevice"]`); err == nil {
			_ = rememberEl.Click(proto.InputMouseButtonLeft, 1)
			log.Printf("[UHCDental] checked Remember Device")
		}
	}

	continueEl, err := page.Timeout(5 * time.Second).Element(`#continuebtn`)
	if err != nil {
		return fmt.Errorf("find #continuebtn: %w", err)
	}
	if err := continueEl.Click(proto.InputMouseButtonLeft, 1); err != nil {
		return fmt.Errorf("click #continuebtn: %w", err)
	}

	log.Printf("[UHCDental] MFA OTP submitted")
	return nil
}

func waitVisible(page *rod.Page, selector string, timeout time.Duration) bool {
	el, err := page.Timeout(timeout).Element(selector)
	if err != nil {
		return false
	}
	visible, err := el.Visible()
	return err == nil && visible
}
