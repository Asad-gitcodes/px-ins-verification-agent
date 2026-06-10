package browser

import (
	"fmt"
	"log"
	"time"

	"github.com/go-rod/rod"
	rodInput "github.com/go-rod/rod/lib/input"
	"github.com/go-rod/rod/lib/proto"
)

const (
	providerBtnSel    = `[data-testid="member-search_provider_select-btn"]`
	providerSearchSel = `[data-testid="member-search_provider_list_search-input_input"]`
	providerOptionSel = `[data-testid="member-search_provider_list"] li[role="option"]`
)

func SelectDashboardProvider(page *rod.Page, providerName string) error {
	if page == nil {
		return fmt.Errorf("browser page is not initialized")
	}

	btn, err := page.Timeout(10 * time.Second).Element(providerBtnSel)
	if err != nil {
		return fmt.Errorf("provider button not found: %w", err)
	}

	// Open dropdown — built-in Click handles scroll, DOM click and keyboard as fallbacks.
	if err := btn.Click(proto.InputMouseButtonLeft, 1); err != nil {
		log.Printf("[DentaQuest] provider btn.Click failed: %v", err)
	}
	if !waitVisible(page, providerOptionSel, 2*time.Second) {
		if _, err := btn.Eval(`() => this.click()`); err != nil {
			log.Printf("[DentaQuest] provider DOM click failed: %v", err)
		}
		if !waitVisible(page, providerOptionSel, 2*time.Second) {
			if err := btn.Focus(); err == nil {
				_ = page.Keyboard.Press(rodInput.Enter)
			}
			if !waitVisible(page, providerOptionSel, 2*time.Second) {
				return fmt.Errorf("provider dropdown did not open for %q", providerName)
			}
		}
	}

	// Type into search if present; some dropdown states skip it.
	if searchInput, err := page.Timeout(500 * time.Millisecond).Element(providerSearchSel); err == nil {
		if err := searchInput.Input(providerName); err != nil {
			return fmt.Errorf("fill provider search: %w", err)
		}
		time.Sleep(800 * time.Millisecond) // wait for debounced results
	}

	// Click the first matching option.
	option, err := page.Timeout(5 * time.Second).Element(providerOptionSel)
	if err != nil {
		return fmt.Errorf("no provider options for %q: %w", providerName, err)
	}
	if err := option.Click(proto.InputMouseButtonLeft, 1); err != nil {
		_, _ = option.Eval(`() => this.click()`)
	}

	// Confirm dropdown closed.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		ariaExpanded, _ := btn.Attribute("aria-expanded")
		if ariaExpanded != nil && *ariaExpanded == "false" {
			return nil
		}
		if !waitVisible(page, providerOptionSel, 300*time.Millisecond) {
			return nil
		}
	}
	return fmt.Errorf("provider dropdown did not close after selecting %q", providerName)
}
