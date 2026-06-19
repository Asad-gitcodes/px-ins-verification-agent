package browser

import (
	"fmt"
	"log"
	"strings"
	"time"

	"insurance-benefit-agent-go/internal/payers"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/proto"
	rodInput "github.com/go-rod/rod/lib/input"
)

func runPortalSignIn(page *rod.Page, sessionInput payers.SessionInput, shouldGoto bool, submit bool) (bool, error) {
	if shouldGoto {
		if err := page.Navigate(loginURL); err != nil {
			return false, fmt.Errorf("open DentaQuest portal login: %w", err)
		}
		log.Printf("[DentaQuest] loaded portal sign-in: %s", currentURL(page))
	} else {
		log.Printf("[DentaQuest] using current page as portal sign-in: %s", currentURL(page))
	}

	// The portal sometimes detects saved cookies and starts an SSO flow in
	// another tab, showing a blocking dialog on the current page. Dismiss it.
	dismissAuthInAnotherTabDialog(page)

	const usernameSel = `[data-testid="username"], input[name="username"], input[placeholder*="username" i]`
	if !waitVisible(page, usernameSel, 10*time.Second) {
		log.Printf("[DentaQuest] portal sign-in form did not render; continuing")
		return false, nil
	}
	usernameEl, err := page.Element(usernameSel)
	if err != nil {
		return false, fmt.Errorf("find DentaQuest portal username: %w", err)
	}
	if err := usernameEl.Input(sessionInput.Credential.Username); err != nil {
		return false, fmt.Errorf("fill DentaQuest portal username: %w", err)
	}

	const passwordSel = `[data-testid="password"], input[name="password"], input[type="password"]`
	if !waitVisible(page, passwordSel, 10*time.Second) {
		return false, fmt.Errorf("wait for DentaQuest portal password: timed out")
	}
	passwordEl, err := page.Element(passwordSel) // fresh ref — no inherited timeout
	if err != nil {
		return false, fmt.Errorf("find DentaQuest portal password: %w", err)
	}
	if err := passwordEl.Input(sessionInput.Password); err != nil {
		return false, fmt.Errorf("fill DentaQuest portal password: %w", err)
	}

	tryEnsureRememberMeChecked(page)
	if !submit {
		log.Printf("[DentaQuest] portal sign-in form prepared without submit")
		return true, nil
	}

	const signInSel = `button[type="submit"], [aria-label="Sign in"]`
	if !waitVisible(page, signInSel, 10*time.Second) {
		return false, fmt.Errorf("wait for DentaQuest portal sign-in button: timed out")
	}
	signInEl, err := page.Element(signInSel) // fresh ref — no inherited timeout
	if err != nil {
		return false, fmt.Errorf("find DentaQuest portal sign-in button: %w", err)
	}
	if clickErr := signInEl.Click(proto.InputMouseButtonLeft, 1); clickErr != nil {
		// "context canceled" means the click triggered page navigation — that's
		// success, not failure. Only fall back to Enter for other errors.
		if strings.Contains(clickErr.Error(), "context canceled") {
			log.Printf("[DentaQuest] portal sign-in click triggered navigation (context canceled) — proceeding")
		} else {
			log.Printf("[DentaQuest] portal sign-in click failed; pressing Enter on password field")
			// Re-fetch password field — original ref may be stale after a click attempt.
			if fresh, err := page.Element(passwordSel); err == nil {
				passwordEl = fresh
			}
			if focusErr := passwordEl.Focus(); focusErr != nil {
				return false, fmt.Errorf("submit DentaQuest portal sign-in: click=%v focus=%w", clickErr, focusErr)
			}
			if pressErr := page.Keyboard.Press(rodInput.Enter); pressErr != nil {
				return false, fmt.Errorf("submit DentaQuest portal sign-in: click=%v press=%w", clickErr, pressErr)
			}
		}
	}

	time.Sleep(1500 * time.Millisecond)
	log.Printf("[DentaQuest] submitted provider portal sign-in: %s", currentURL(page))
	return true, nil
}

// dismissAuthInAnotherTabDialog detects the "Authentication flow continued in
// another tab" modal that the portal shows when it detects stale cookies and
// kicks off an SSO flow. Clicks Ok to dismiss it so the sign-in form is usable.
func dismissAuthInAnotherTabDialog(page *rod.Page) {
	// The dialog appears within ~3 seconds of page load when SSO is triggered.
	el, err := page.Timeout(3 * time.Second).Element(`[role="dialog"] button[aria-label="Ok"], dialog button[aria-label="Ok"], button[aria-label="Ok"]`)
	if err != nil {
		return // no dialog — normal login page
	}
	log.Printf("[DentaQuest] dismissing 'auth in another tab' dialog")
	_ = el.Click(proto.InputMouseButtonLeft, 1)
	time.Sleep(time.Second)
}

func tryEnsureRememberMeChecked(page *rod.Page) {
	el, err := page.Element(`input[name="rememberMe"][type="checkbox"]`)
	if err != nil {
		return
	}
	if visible, err := el.Visible(); err != nil || !visible {
		return
	}
	checked, err := el.Property("checked")
	if err == nil && checked.Bool() {
		return
	}
	if _, err := el.Eval("() => this.click()"); err != nil {
		log.Printf("[DentaQuest] failed to click portal Remember me checkbox: %v", err)
	}
}
