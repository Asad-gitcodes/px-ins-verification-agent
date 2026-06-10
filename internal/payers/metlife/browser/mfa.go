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

var metLifeSMSPattern = regexp.MustCompile(`\b(\d{6})\b`)

func handleMFA(page *rod.Page, input payers.SessionInput) error {
	method := strings.ToLower(strings.TrimSpace(input.Credential.MFAMethod))
	if method == "" {
		method = "sms"
	}

	if strings.Contains(method, "email") {
		return completeEmailMFA(page, input)
	}
	return completeSMSMFA(page, input)
}

func completeSMSMFA(page *rod.Page, input payers.SessionInput) error {
	if err := selectMFATile(page, "Text Message"); err != nil {
		return err
	}

	codeSentAt := time.Now()

	smsCfg, err := mfa.SMSConfigFromScraperConfig(input.ScraperConfig, input.RequestedOfficeKey)
	if err != nil {
		return fmt.Errorf("MetLife SMS config: %w", err)
	}
	code, err := mfa.GetSmsCodeByPattern(*smsCfg, codeSentAt, metLifeSMSPattern)
	if err != nil {
		return fmt.Errorf("get MetLife SMS code: %w", err)
	}
	return submitMFACode(page, code)
}

func selectMFATile(page *rod.Page, label string) error {
	if page == nil {
		return fmt.Errorf("page is nil")
	}

	// Find all MFA tiles
	tiles, err := page.Elements(`div.tile-button.mfa1-input-field`)
	if err != nil {
		return fmt.Errorf("find MFA tiles: %w", err)
	}
	if len(tiles) == 0 {
		return fmt.Errorf("no MFA tiles found")
	}

	var target *rod.Element

	// Pick the tile whose visible text contains the label
	for _, tile := range tiles {
		txt, err := tile.Text()
		if err != nil {
			continue
		}
		if strings.Contains(strings.ToLower(txt), strings.ToLower(label)) {
			target = tile
			break
		}
	}

	if target == nil {
		return fmt.Errorf("MFA tile with label %q not found", label)
	}

	// Scroll into view first
	if err := target.ScrollIntoView(); err != nil {
		return fmt.Errorf("scroll MFA tile into view: %w", err)
	}

	// Small pause can help on flaky MFA UIs
	time.Sleep(300 * time.Millisecond)

	// Try clicking the tile itself first
	if err := target.Click(proto.InputMouseButtonLeft, 1); err == nil {
		return nil
	}

	// Try clicking a child region inside the tile
	for _, selector := range []string{
		`.tile-button__content-container`,
		`.tile-button__title`,
		`.tile-button__icon-container`,
	} {
		child, err := target.Element(selector)
		if err != nil {
			continue
		}
		_ = child.ScrollIntoView()
		if err := child.Click(proto.InputMouseButtonLeft, 1); err == nil {
			return nil
		}
	}

	// Final fallback:
	// Read the inline onclick attribute and execute selectDevice directly.
	onclick, err := target.Attribute("onclick")
	if err != nil {
		return fmt.Errorf("read onclick attribute: %w", err)
	}
	if onclick == nil || *onclick == "" {
		return fmt.Errorf("onclick attribute missing on MFA tile")
	}

	// Extract device ID from:
	// selectDevice(this, event, '47c23e86-96c5-414e-92a6-e0c480573ec7')
	re := regexp.MustCompile(`selectDevice\(\s*this\s*,\s*event\s*,\s*'([^']+)'\s*\)`)
	matches := re.FindStringSubmatch(*onclick)
	if len(matches) < 2 {
		return fmt.Errorf("could not parse device id from onclick: %s", *onclick)
	}
	deviceID := matches[1]

	// Call the page function directly with the actual tile element
	_, err = page.Eval(`
		(tile, deviceID) => {
			if (typeof selectDevice !== "function") {
				throw new Error("selectDevice is not defined");
			}
			selectDevice(tile, null, deviceID);
			return true;
		}
	`, target, deviceID)
	if err != nil {
		return fmt.Errorf("invoke selectDevice directly: %w", err)
	}

	return nil
}

// func clickMFATile(tile *rod.Element) error {
// 	if tile == nil {
// 		return fmt.Errorf("MFA tile is nil")
// 	}
// 	for _, selector := range []string{
// 		`.tile-button__content-container`,
// 		`.tile-button__title`,
// 		`.tile-button__icon-container`,
// 	} {
// 		child, err := tile.Element(selector)
// 		if err != nil {
// 			continue
// 		}
// 		if err := child.Click(proto.InputMouseButtonLeft, 1); err == nil {
// 			return nil
// 		}
// 	}
// 	if err := tile.Click(proto.InputMouseButtonLeft, 1); err == nil {
// 		return nil
// 	}
// 	_, err := tile.Eval(`() => { this.click(); return true }`)
// 	if err != nil {
// 		return err
// 	}
// 	return nil
// }

func completeEmailMFA(page *rod.Page, input payers.SessionInput) error {
	if input.EmailMFA == nil {
		return fmt.Errorf("MetLife email MFA requested but email config is missing")
	}
	if err := selectMFATile(page, "Email 1"); err != nil {
		return err
	}
	codeRequestedAt := time.Now()
	code, err := mfa.GetEmailCode(*input.EmailMFA, codeRequestedAt)
	if err != nil {
		return fmt.Errorf("get MetLife email code: %w", err)
	}
	return submitMFACode(page, code)
}

func submitMFACode(page *rod.Page, code string) error {
	field, err := page.Timeout(20 * time.Second).Element(`input[autocomplete="one-time-code"], input[name*="code" i], input[id*="code" i], input[inputmode="numeric"]`)
	if err != nil {
		return fmt.Errorf("MetLife MFA code input not found: %w", err)
	}
	if err := field.Input(code); err != nil {
		return fmt.Errorf("fill MetLife MFA code: %w", err)
	}

	verifyButton, err := page.Timeout(10 * time.Second).Element(`#sign-on`)
	if err != nil {
		verifyButton, err = page.Timeout(10*time.Second).ElementR(`button, input[type="submit"]`, `Sign On|Verify|Continue|Submit`)
	}
	if err != nil {
		return fmt.Errorf("MetLife MFA submit button not found: %w", err)
	}
	if err := verifyButton.Click(proto.InputMouseButtonLeft, 1); err != nil {
		return fmt.Errorf("click MetLife MFA submit: %w", err)
	}
	return nil
}

func waitForMFACodeInput(page *rod.Page, timeout time.Duration) bool {
	return waitVisible(page, `input[autocomplete="one-time-code"], input[name*="code" i], input[id*="code" i], input[inputmode="numeric"]`, timeout)
}
