package browser

import (
	"log"
	"time"

	"github.com/go-rod/rod"
)

func waitForAuthenticatedPortalContent(page *rod.Page, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if hasAuthenticatedPortalContent(page) {
			return true
		}
		time.Sleep(250 * time.Millisecond)
	}
	return false
}

func hasAuthenticatedPortalContent(page *rod.Page) bool {
	// Best-effort wait for network to settle before checking for nav element.
	_ = page.Timeout(3 * time.Second).WaitLoad()

	for _, sel := range []string{
		`[data-testid="member-search-form-container"]`,
		`[data-testid="main-navigation"]`,
		`[data-testid="navigation-item-members"]`,
		`button[aria-label*="TIN"]`,
		`button[aria-label*="mfa patientxpress"]`,
		`button[aria-label="Search"]`,
		`input[placeholder*="Search by member ID"]`,
	} {
		if waitVisible(page, sel, 500*time.Millisecond) {
			log.Printf("[DentaQuest] authenticated portal content loaded using selector=%s", sel)
			return true
		}
	}

	if _, err := page.Timeout(750 * time.Millisecond).ElementR(`body, main, div`, `(?i)search for a member\s*&\s*check eligibility|welcome back`); err == nil {
		log.Printf("[DentaQuest] authenticated portal content loaded using text content")
		return true
	}
	return false
}

// waitVisible waits up to timeout for selector to be visible in the DOM.
func waitVisible(page *rod.Page, selector string, timeout time.Duration) bool {
	el, err := page.Timeout(timeout).Element(selector)
	if err != nil {
		return false
	}
	visible, err := el.Visible()
	return err == nil && visible
}

// isVisible reports whether selector matches a currently visible element.
func isVisible(page *rod.Page, selector string) bool {
	el, err := page.Element(selector)
	if err != nil {
		return false
	}
	visible, err := el.Visible()
	return err == nil && visible
}
