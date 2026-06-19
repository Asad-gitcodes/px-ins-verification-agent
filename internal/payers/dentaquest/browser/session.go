package browser

import (
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	sharedbrowser "insurance-benefit-agent-go/internal/browser"
	"insurance-benefit-agent-go/internal/payers"

	"github.com/go-rod/rod"
)

const (
	loginURL     = "https://providers.dentaquest.com/onboarding/start/"
	dashboardURL = "https://providers.dentaquest.com/dashboard/"
)

type Session struct {
	browser              *sharedbrowser.Session
	storageStatePath     string
	hasStorageState      bool
	payloads             map[string]*CapturedPayload
	payloadsMu           sync.Mutex
	lastPayloadAt        time.Time
	captureMemberDetails bool
}

func Launch(input payers.SessionInput) (*Session, error) {
	storageStatePath := storageStatePathFor(input)
	_, statErr := os.Stat(storageStatePath)
	hasStorageState := statErr == nil
	if statErr != nil && !os.IsNotExist(statErr) {
		return nil, fmt.Errorf("stat DentaQuest auth session: %w", statErr)
	}

	browserSession, err := sharedbrowser.Launch(sharedbrowser.LaunchOptions{
		StorageStatePath: storageStatePath,
		Headless:         input.Headless,
	})
	if err != nil {
		return nil, err
	}

	s := &Session{
		browser:          browserSession,
		storageStatePath: storageStatePath,
		hasStorageState:  hasStorageState,
		payloads:         make(map[string]*CapturedPayload),
	}
	s.startNetworkCapture()
	s.enableNetworkCaptureOnPage(s.browser.Page)
	return s, nil
}

func (s *Session) Close() error {
	if s == nil || s.browser == nil {
		return nil
	}
	return s.browser.Close()
}

func (s *Session) Page() *rod.Page {
	if s == nil || s.browser == nil {
		return nil
	}
	return s.browser.Page
}

func (s *Session) Browser() *rod.Browser {
	if s == nil || s.browser == nil {
		return nil
	}
	return s.browser.Browser
}

// Login always starts from onboarding and resolves one of three states:
// 1. cached session handoff to dashboard
// 2. portal sign-in form
// 3. MFA prompt after sign-in
func (s *Session) Login(input payers.SessionInput) error {
	if s == nil || s.browser == nil || s.browser.Page == nil {
		return fmt.Errorf("browser session is not initialized")
	}

	page := s.browser.Page

	if err := page.Navigate(loginURL); err != nil {
		return fmt.Errorf("open DentaQuest onboarding start: %w", err)
	}

	if err := s.resolveOnboardingState(page, input); err != nil {
		return err
	}

	if err := s.waitForLoginComplete(page, input); err != nil {
		return err
	}

	if err := s.browser.SaveStorageState(s.storageStatePath); err != nil {
		return err
	}
	return nil
}

func (s *Session) resolveOnboardingState(page *rod.Page, input payers.SessionInput) error {
	if hasAuthenticatedPortalContent(page) {
		return nil
	}

	if authMovedToAnotherTab(page) {
		dismissAuthInAnotherTabDialog(page)
		return nil
	}

	submitted, err := runPortalSignIn(page, input, false, true)
	if err != nil {
		return err
	}
	if submitted {
		return nil
	}

	if isEmailMFAPromptVisible(page) {
		return nil
	}

	if hasAuthenticatedPortalContent(page) {
		return nil
	}

	return fmt.Errorf("DentaQuest onboarding did not show auth handoff, sign-in form, or dashboard: %s", currentURL(page))
}

// waitForLoginComplete polls until the dashboard is reachable either on the
// current page or in a new tab. Handles MFA if the portal asks for it.
func (s *Session) waitForLoginComplete(page *rod.Page, input payers.SessionInput) error {
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		if hasAuthenticatedPortalContent(page) {
			return nil
		}

		if err := s.adoptDashboardPage(); err == nil {
			return nil
		}

		if authMovedToAnotherTab(page) {
			dismissAuthInAnotherTabDialog(page)
		}

		if isEmailMFAPromptVisible(page) {
			if err := handleEmailMFA(page, input); err != nil {
				return err
			}
			continue
		}

		time.Sleep(time.Second)
	}
	return fmt.Errorf("DentaQuest login did not reach dashboard: %s", currentURL(page))
}

func authMovedToAnotherTab(page *rod.Page) bool {
	if page == nil {
		return false
	}
	_, err := page.Timeout(500*time.Millisecond).ElementR(`body, [role="dialog"]`, `(?i)authentication flow continued in another tab`)
	return err == nil
}

func (s *Session) adoptDashboardPage() error {
	if s == nil || s.browser == nil || s.browser.Browser == nil {
		return fmt.Errorf("browser session is not initialized")
	}

	pages, err := s.browser.Browser.Pages()
	if err != nil {
		return fmt.Errorf("list browser pages: %w", err)
	}
	if len(pages) == 0 {
		return fmt.Errorf("no browser pages found")
	}

	var dashboardPage *rod.Page
	for _, candidate := range pages {
		if dashboardPage == nil && hasAuthenticatedPortalContent(candidate) {
			dashboardPage = candidate
		}
	}
	if dashboardPage == nil {
		return fmt.Errorf("no dashboard tab found")
	}

	s.browser.Page = dashboardPage
	s.enableNetworkCaptureOnPage(dashboardPage)
	if _, err := dashboardPage.Activate(); err != nil {
		log.Printf("[DentaQuest] activate dashboard tab failed: %v", err)
	}
	return nil
}

func storageStatePathFor(input payers.SessionInput) string {
	return fmt.Sprintf("auth-%s-%s-slot-%s.json",
		slug(input.Payer.PayerURL),
		input.RequestedOfficeKey,
		strconv.Itoa(input.Credential.Slot),
	)
}

func slug(value string) string {
	var builder strings.Builder
	for _, r := range strings.ToLower(value) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			builder.WriteRune(r)
		}
	}
	if builder.Len() == 0 {
		return "payer"
	}
	return builder.String()
}

func currentURL(page *rod.Page) string {
	if page == nil {
		return ""
	}
	info, err := page.Info()
	if err != nil {
		return ""
	}
	return info.URL
}
