package emblemhealth

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"insurance-benefit-agent-go/internal/advanced"
	agentbrowser "insurance-benefit-agent-go/internal/browser"
	"insurance-benefit-agent-go/internal/mfa"
	"insurance-benefit-agent-go/internal/models"
	"insurance-benefit-agent-go/internal/payers"
	emapi "insurance-benefit-agent-go/internal/payers/emblemhealth/api"
	emeligibility "insurance-benefit-agent-go/internal/payers/emblemhealth/eligibility"
	"insurance-benefit-agent-go/internal/resultwriter"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/proto"
)

const (
	PayerURL           = "EmblemHealth.com"
	providerBaseURL    = "https://provider.emblemhealth.com"
	loginURL           = providerBaseURL + "/ehprovider/providerlogin?ec=302&startURL=%2Fehprovider%2Fs%2F"
	dashboardURL       = providerBaseURL + "/ehprovider/s/"
	eligibilityPageURL = providerBaseURL + "/ehprovider/s/bulk-eligibility-report"
	apexRemoteURL      = providerBaseURL + "/ehprovider/apexremote"
	apexAction         = "vlocity_ins.CardCanvasController.doGenericInvoke"
	bulkMethod         = "EligibilityReports_MultipleMemberSearch"
)

type emblemApexCtx struct {
	Authorization string `json:"authorization"`
	CSRF          string `json:"csrf"`
	NS            string `json:"ns"`
	Ver           int    `json:"ver"`
	VID           string `json:"vid"`
}

type Adapter struct{}

func NewAdapter() *Adapter { return &Adapter{} }

func (a *Adapter) PayerURL() string { return PayerURL }

func (a *Adapter) Supports(payerURL string) bool {
	switch strings.ToLower(strings.TrimSpace(payerURL)) {
	case strings.ToLower(PayerURL), "emblemhealth", "emblem health", "provider.emblemhealth.com":
		return true
	default:
		return false
	}
}

func (a *Adapter) Run(ctx context.Context, input payers.SessionInput) (payers.RunSummary, error) {
	if input.SkipProbing {
		return a.runPhase2Only(ctx, input)
	}
	return a.runPhase1(ctx, input)
}

func (a *Adapter) runPhase1(ctx context.Context, input payers.SessionInput) (payers.RunSummary, error) {
	var summary payers.RunSummary
	if len(input.Appointments) == 0 {
		return summary, fmt.Errorf("[EmblemHealth] session requires at least one appointment")
	}
	probeDir := input.ProbeOutputDir
	if probeDir == "" {
		probeDir = payers.ProbeRunDir("")
	}
	if err := os.MkdirAll(probeDir, 0o755); err != nil {
		return summary, fmt.Errorf("[EmblemHealth] create probe dir: %w", err)
	}

	session, err := launchAndLogin(input)
	if err != nil {
		return summary, err
	}
	browserClosed := false
	closeBrowser := func() {
		if browserClosed {
			return
		}
		browserClosed = true
		if closeErr := session.Close(); closeErr != nil {
			log.Printf("[EmblemHealth] browser close failed: %v", closeErr)
		}
	}
	defer closeBrowser()

	// Install the response hook in every new document/iframe so it survives any navigation
	// that occurs when clicking a member result row.
	if _, err := session.Page.EvalOnNewDocument(apexResponseHookJS()); err != nil {
		log.Printf("[EmblemHealth] EvalOnNewDocument hook warning: %v", err)
	}
	// Also patch any already-loaded iframes on the current page.
	if err := installApexRemoteResponseHook(session.Page); err != nil {
		log.Printf("[EmblemHealth] response hook install warning: %v", err)
	}

	chunks := chunkAppointments(input.Appointments, 10)
	log.Printf("[EmblemHealth] phase 1: %d patients in %d chunks", len(input.Appointments), len(chunks))

	for chunkIdx, chunk := range chunks {
		select {
		case <-ctx.Done():
			return summary, ctx.Err()
		default:
		}

		if chunkIdx > 0 {
			if waitErr := waitForEmblemEligibilityPage(session.Page, 3*time.Second); waitErr != nil {
				if navErr := navigateToEmblemEligibilityPage(session.Page); navErr != nil {
					log.Printf("[EmblemHealth] re-navigate failed: %v", navErr)
				}
				_ = installApexRemoteResponseHook(session.Page)
			}
			resetEligibilityForm(session.Page)
		}

		memberIDs := make([]string, 0, len(chunk))
		for _, appt := range chunk {
			memberIDs = append(memberIDs, strings.TrimSpace(appt.SubscriberID))
		}

		clearCapturedApexResponse(session.Page)

		if err := fillAndClickEligibilityForm(session.Page, strings.Join(memberIDs, ","), 15*time.Second); err != nil {
			log.Printf("[EmblemHealth] chunk %d form fill failed: %v", chunkIdx, err)
			for _, appt := range chunk {
				writeProbeError(probeDir, input.Payer.PayerURL, appt, err)
			}
			continue
		}

		raw, err := waitForCapturedApexResponse(session.Page, 60*time.Second)
		if err != nil {
			log.Printf("[EmblemHealth] chunk %d response timeout: %v", chunkIdx, err)
			for _, appt := range chunk {
				writeProbeError(probeDir, input.Payer.PayerURL, appt, err)
			}
			continue
		}

		result, parseErr := emapi.ParseApexResult(raw)
		if parseErr != nil {
			log.Printf("[EmblemHealth] chunk %d parse failed: %v", chunkIdx, parseErr)
			for _, appt := range chunk {
				writeProbeError(probeDir, input.Payer.PayerURL, appt, parseErr)
			}
			continue
		}

		recordsByID := emapi.MatchRecordsByMemberID(result)
		now := time.Now().UTC().Format(time.RFC3339)
		for _, appt := range chunk {
			memberID := strings.TrimSpace(appt.SubscriberID)
			record, ok := recordsByID[strings.ToUpper(memberID)]
			bundle := &emapi.ProbeBundle{
				PayerURL:          input.Payer.PayerURL,
				RequestedMemberID: memberID,
				Appointment:       appt,
				RawResult:         result,
				RecordedAt:        now,
			}
			if ok {
				bundle.Record = &record
				// Detail capture disabled — office goal is active/not-active only.
				// To re-enable full benefit collection, restore the commented-out
				// detail capture block from git history.
			}
			if writeErr := writeProbeBundle(probeDir, input.Payer.PayerURL, appt, bundle); writeErr != nil {
				log.Printf("[EmblemHealth] write probe failed patNum=%s: %v", appt.PatNum, writeErr)
				continue
			}
			active := ok && strings.EqualFold(strings.TrimSpace(record.Status), "Active")
			log.Printf("[EmblemHealth] %s %s memberID=%s active=%t", appt.PatNum, appt.AptNum, memberID, active)
		}
	}
	closeBrowser()
	log.Printf("[EmblemHealth] phase 1 done")
	return summary, nil
}

func launchAndLogin(input payers.SessionInput) (*agentbrowser.Session, error) {
	storagePath := fmt.Sprintf("auth-emblemhealth-com-%s-slot-%d.json",
		payers.SanitizeProbeSegment(input.RequestedOfficeKey), input.Credential.Slot)
	session, err := agentbrowser.Launch(agentbrowser.LaunchOptions{
		StorageStatePath: storagePath,
		Headless:         input.Headless,
	})
	if err != nil {
		return nil, fmt.Errorf("[EmblemHealth] launch browser: %w", err)
	}
	page := session.Page
	if err := page.Navigate(loginURL); err != nil {
		_ = session.Close()
		return nil, fmt.Errorf("[EmblemHealth] navigate login page: %w", err)
	}
	waitForEmblemPageSettle(page)
	if err := waitForLoginOrSession(page, 45*time.Second); err != nil {
		_ = session.Close()
		return nil, err
	}
	if err := maybeLogin(page, input.Credential.Username, input.Credential.Password); err != nil {
		_ = session.Close()
		return nil, err
	}
	if err := settleLoginFlow(page, input); err != nil {
		_ = session.Close()
		return nil, err
	}
	if err := waitForEmblemDashboard(page, 90*time.Second); err != nil {
		_ = session.Close()
		return nil, err
	}
	if err := page.Navigate(emblemDashboardURLForCurrentPage(page)); err != nil {
		_ = session.Close()
		return nil, fmt.Errorf("[EmblemHealth] navigate dashboard: %w", err)
	}
	waitForEmblemPageSettle(page)
	if err := waitForEmblemDashboard(page, 60*time.Second); err != nil {
		_ = session.Close()
		return nil, err
	}
	if err := navigateToEmblemEligibilityPage(page); err != nil {
		_ = session.Close()
		return nil, err
	}
	if err := session.SaveStorageState(storagePath); err != nil {
		log.Printf("[EmblemHealth] storage state save skipped: %v", err)
	}
	return session, nil
}

func waitForLoginOrSession(page *rod.Page, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := page.Timeout(500 * time.Millisecond).Element(`#password, input[type="password"]`); err == nil {
			log.Printf("[EmblemHealth] login form ready")
			return nil
		}
		if isEmblemLoggedIn(page) {
			log.Printf("[EmblemHealth] existing session detected")
			return nil
		}
		if isLoginFlowPage(page) {
			log.Printf("[EmblemHealth] login flow page detected")
			return nil
		}
		time.Sleep(500 * time.Millisecond)
	}
	return fmt.Errorf("[EmblemHealth] login page did not settle")
}

func isEmblemLoggedIn(page *rod.Page) bool {
	res, err := page.Eval(`() => {
		const url = String(location.href || '');
		const text = document.body ? document.body.innerText : '';
		if (url.includes('/loginflow/') ||
			url.includes('/setup/secur/RemoteAccessAuthorizationPage') ||
			/Finish Logging In|Request Code|Two-Step Verification|Email Address|Remote Access Authorization|Allow Access/i.test(text)) {
			return false;
		}
		return url.includes('/ehprovider/s/') ||
			/Bulk Eligibility|Eligibility|Dashboard|Sign Out|Log Out/i.test(text);
	}`)
	return err == nil && res.Value.Bool()
}

func maybeLogin(page *rod.Page, username, password string) error {
	if _, err := page.Timeout(5 * time.Second).Element(`input[type="password"], #password`); err != nil {
		log.Printf("[EmblemHealth] login form not present; continuing with existing session")
		return nil
	}
	userEl, err := firstElement(page, []string{
		`#username`,
		`input[name="username"]`,
		`input[id*="username" i]`,
		`input[type="email"]`,
		`input[type="text"]`,
	})
	if err != nil {
		return fmt.Errorf("[EmblemHealth] login username field not found: %w", err)
	}
	passEl, err := firstElement(page, []string{
		`#password`,
		`input[type="password"]`,
		`input[name="password"]`,
		`input[id*="password" i]`,
	})
	if err != nil {
		return fmt.Errorf("[EmblemHealth] login password field not found: %w", err)
	}
	if err := userEl.Input(username); err != nil {
		return fmt.Errorf("[EmblemHealth] fill username: %w", err)
	}
	if err := passEl.Input(password); err != nil {
		return fmt.Errorf("[EmblemHealth] fill password: %w", err)
	}
	button, err := firstElement(page, []string{
		`#search`,
		`button[type="submit"]`,
		`input[type="submit"]`,
		`button`,
	})
	if err != nil {
		return fmt.Errorf("[EmblemHealth] login submit not found: %w", err)
	}
	if err := button.Click(proto.InputMouseButtonLeft, 1); err != nil {
		return fmt.Errorf("[EmblemHealth] click login: %w", err)
	}
	waitForEmblemPageSettle(page)
	return nil
}

func waitForEmblemPageSettle(page *rod.Page) {
	_ = page.WaitLoad()
	time.Sleep(5 * time.Second)
}

func waitForEmblemDashboard(page *rod.Page, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	stableStreak := 0
	for time.Now().Before(deadline) {
		res, err := page.Eval(`() => {
			const current = new URL(location.href);
			const text = document.body ? document.body.innerText : '';
			const stableURL = current.origin === 'https://provider.emblemhealth.com' &&
				current.pathname === '/ehprovider/s/' &&
				current.searchParams.has('sfdc_community_url') &&
				current.searchParams.has('sfdc_community_id') &&
				!current.searchParams.has('code') &&
				!current.searchParams.has('state');
			const links = Array.from(document.querySelectorAll('a.action-box, a[ng-click], a, [role="button"]'));
			const eligibilityAction = links.find(el => /Check Member Eligibility/i.test(el.innerText || el.textContent || ''));
			const visibleEligibilityAction = !!eligibilityAction &&
				!eligibilityAction.disabled &&
				(eligibilityAction.offsetParent !== null || eligibilityAction.getClientRects().length > 0);
			return { stableURL, linkReady: visibleEligibilityAction || /Check Member Eligibility/i.test(text) };
		}`)
		if err == nil {
			stableURL := res.Value.Get("stableURL").Bool()
			linkReady := res.Value.Get("linkReady").Bool()
			if stableURL && linkReady {
				log.Printf("[EmblemHealth] logged in")
				time.Sleep(5 * time.Second)
				return nil
			}
			if stableURL {
				stableStreak++
				if stableStreak >= 3 {
					log.Printf("[EmblemHealth] logged in")
					return nil
				}
			} else {
				stableStreak = 0
			}
		}
		time.Sleep(1 * time.Second)
	}
	return fmt.Errorf("[EmblemHealth] dashboard did not settle: %s", emblemPageState(page))
}

func waitForEmblemEligibilityPage(page *rod.Page, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		res, err := page.Eval(`() => {
			const url = String(location.href || '');
			const text = document.body ? document.body.innerText : '';
			return url.includes('/bulk-eligibility-report') ||
				/Bulk Eligibility|bulk eligibility|bulkeligiblityMemberID/i.test(text) ||
				!!document.querySelector('#bulkeligiblityMemberID');
		}`)
		if err == nil && res.Value.Bool() {
			return nil
		}
		time.Sleep(500 * time.Millisecond)
	}
	return fmt.Errorf("[EmblemHealth] bulk eligibility page did not settle: %s", emblemPageState(page))
}

func navigateToEmblemEligibilityPage(page *rod.Page) error {
	targetURL := emblemEligibilityURLForCurrentPage(page)
	if err := page.Navigate(targetURL); err != nil {
		return fmt.Errorf("[EmblemHealth] navigate eligibility page: %w", err)
	}
	waitForEmblemPageSettle(page)
	if isEmblemDashboardPage(page) {
		log.Printf("[EmblemHealth] direct bulk eligibility route bounced to dashboard; trying portal navigation")
	} else if err := waitForEmblemEligibilityPage(page, 8*time.Second); err == nil {
		return nil
	}
	log.Printf("[EmblemHealth] direct bulk eligibility route did not settle; trying portal navigation")
	if clicked, err := clickEmblemBulkEligibilityLink(page); err != nil {
		return err
	} else if clicked {
		log.Printf("[EmblemHealth] clicked bulk eligibility link")
		waitForEmblemPageSettle(page)
		return waitForEmblemEligibilityPage(page, 60*time.Second)
	}
	if clicked, err := clickEmblemPortalEntry(page, []string{"check member eligibility", "member eligibility"}); err != nil {
		return err
	} else if clicked {
		log.Printf("[EmblemHealth] opened member eligibility entry from dashboard")
		waitForEmblemPageSettle(page)
		if err := waitForEmblemEligibilityPage(page, 10*time.Second); err == nil {
			return nil
		}
		if clicked, err := clickEmblemBulkEligibilityLink(page); err != nil {
			return err
		} else if clicked {
			log.Printf("[EmblemHealth] clicked bulk eligibility link from member eligibility entry")
			waitForEmblemPageSettle(page)
			return waitForEmblemEligibilityPage(page, 60*time.Second)
		}
		log.Printf("[EmblemHealth] member eligibility entry opened but bulk page not ready; retrying direct route")
		targetURL = emblemEligibilityURLForCurrentPage(page)
		if err := page.Navigate(targetURL); err != nil {
			return fmt.Errorf("[EmblemHealth] navigate eligibility page after entry: %w", err)
		}
		waitForEmblemPageSettle(page)
		return waitForEmblemEligibilityPage(page, 60*time.Second)
	}
	if clicked, err := clickButtonByText(page, "member management"); err != nil {
		return fmt.Errorf("[EmblemHealth] open Member Management menu: %w", err)
	} else if clicked {
		log.Printf("[EmblemHealth] opened Member Management menu")
		time.Sleep(1 * time.Second)
	}
	if clicked, err := clickEmblemBulkEligibilityLink(page); err != nil {
		return err
	} else if clicked {
		log.Printf("[EmblemHealth] clicked bulk eligibility link from Member Management")
		waitForEmblemPageSettle(page)
		return waitForEmblemEligibilityPage(page, 60*time.Second)
	}
	return fmt.Errorf("[EmblemHealth] bulk eligibility link not found from dashboard: %s", emblemPageState(page))
}

func emblemDashboardURLForCurrentPage(page *rod.Page) string {
	res, err := page.Eval(`(baseURL) => {
		const current = new URL(location.href);
		const target = new URL(baseURL);
		for (const key of ['sfdc_community_url', 'sfdc_community_id']) {
			const value = current.searchParams.get(key);
			if (value) target.searchParams.set(key, value);
		}
		return target.toString();
	}`, dashboardURL)
	if err != nil {
		return dashboardURL
	}
	return res.Value.Str()
}

func emblemEligibilityURLForCurrentPage(page *rod.Page) string {
	res, err := page.Eval(`(baseURL) => {
		const current = new URL(location.href);
		const target = new URL(baseURL);
		for (const key of ['sfdc_community_url', 'sfdc_community_id']) {
			const value = current.searchParams.get(key);
			if (value) target.searchParams.set(key, value);
		}
		return target.toString();
	}`, eligibilityPageURL)
	if err != nil {
		return eligibilityPageURL
	}
	return res.Value.Str()
}

func isEmblemDashboardPage(page *rod.Page) bool {
	res, err := page.Eval(`() => {
		const current = new URL(location.href);
		const text = document.body ? document.body.innerText : '';
		const links = Array.from(document.querySelectorAll('a.action-box, a[ng-click], a, [role="button"]'));
		const eligibilityAction = links.find(el => /Check Member Eligibility/i.test(el.innerText || el.textContent || ''));
		const visibleEligibilityAction = !!eligibilityAction &&
			!eligibilityAction.disabled &&
			(eligibilityAction.offsetParent !== null || eligibilityAction.getClientRects().length > 0);
		return current.pathname === '/ehprovider/s/' &&
			!current.href.includes('/bulk-eligibility-report') &&
			current.searchParams.has('sfdc_community_url') &&
			current.searchParams.has('sfdc_community_id') &&
			(visibleEligibilityAction || /Check Member Eligibility/i.test(text));
	}`)
	return err == nil && res.Value.Bool()
}

func clickEmblemBulkEligibilityLink(page *rod.Page) (bool, error) {
	res, err := page.Eval(`() => {
		const normalize = (value) => String(value || '').replace(/\s+/g, ' ').trim().toLowerCase();
		const candidates = Array.from(document.querySelectorAll('a, button, [role="button"], [onclick]'));
		for (const el of candidates) {
			const label = normalize([
				el.innerText,
				el.value,
				el.textContent,
				el.getAttribute && el.getAttribute('aria-label'),
				el.getAttribute && el.getAttribute('title'),
				el.getAttribute && el.getAttribute('href')
			].filter(Boolean).join(' '));
			if (!label.includes('bulk-eligibility-report') && !label.includes('bulk eligibility')) continue;
			if (el.disabled) continue;
			el.scrollIntoView({block: 'center', inline: 'center'});
			el.focus && el.focus();
			el.dispatchEvent(new MouseEvent('mousedown', {bubbles: true, cancelable: true, view: window}));
			el.dispatchEvent(new MouseEvent('mouseup', {bubbles: true, cancelable: true, view: window}));
			el.dispatchEvent(new MouseEvent('click', {bubbles: true, cancelable: true, view: window}));
			if (typeof el.click === 'function') el.click();
			return true;
		}
		return false;
	}`)
	if err != nil {
		return false, fmt.Errorf("[EmblemHealth] click bulk eligibility link: %w", err)
	}
	return res.Value.Bool(), nil
}

func clickEmblemPortalEntry(page *rod.Page, labels []string) (bool, error) {
	data, err := json.Marshal(labels)
	if err != nil {
		return false, err
	}
	res, err := page.Eval(`(labelsJSON) => {
		const labels = JSON.parse(labelsJSON).map(v => String(v || '').toLowerCase());
		const normalize = (value) => String(value || '').replace(/\s+/g, ' ').trim().toLowerCase();
		const click = (el) => {
			if (!el || el.disabled) return false;
			el.scrollIntoView({block: 'center', inline: 'center'});
			el.focus && el.focus();
			el.dispatchEvent(new MouseEvent('mousedown', {bubbles: true, cancelable: true, view: window}));
			el.dispatchEvent(new MouseEvent('mouseup', {bubbles: true, cancelable: true, view: window}));
			el.dispatchEvent(new MouseEvent('click', {bubbles: true, cancelable: true, view: window}));
			if (typeof el.click === 'function') el.click();
			return true;
		};
		const bestClickable = (el) => {
			let cur = el;
			for (let i = 0; cur && i < 8; i++, cur = cur.parentElement) {
				const role = (cur.getAttribute && cur.getAttribute('role')) || '';
				if (cur.matches && cur.matches('a, button, [role="button"], [onclick]')) return cur;
				if (role.toLowerCase() === 'button') return cur;
				if (cur.tabIndex >= 0) return cur;
			}
			return el;
		};
		const selectors = 'a.action-box, a[ng-click], a, button, [role="button"], [onclick], div, span, h1, h2, h3, p';
		for (const el of Array.from(document.querySelectorAll(selectors))) {
			const label = normalize([
				el.innerText,
				el.value,
				el.textContent,
				el.getAttribute && el.getAttribute('aria-label'),
				el.getAttribute && el.getAttribute('title'),
				el.getAttribute && el.getAttribute('href')
			].filter(Boolean).join(' '));
			if (!labels.some(needle => label.includes(needle))) continue;
			const actionBox = el.matches && el.matches('a.action-box, a[ng-click]') ? el : el.closest && el.closest('a.action-box, a[ng-click]');
			if (click(actionBox || bestClickable(el))) return true;
		}
		return false;
	}`, string(data))
	if err != nil {
		return false, fmt.Errorf("[EmblemHealth] click portal entry: %w", err)
	}
	return res.Value.Bool(), nil
}

func settleLoginFlow(page *rod.Page, input payers.SessionInput) error {
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		progress := false
		finished, err := finishLoginFlowIfPresent(page)
		if err != nil {
			return err
		}
		if finished {
			progress = true
		}
		completedMFA, err := completeEmailMFAIfPresent(page, input)
		if err != nil {
			return err
		}
		if completedMFA {
			progress = true
		}
		authorized, err := authorizeRemoteAccessIfPresent(page)
		if err != nil {
			return err
		}
		if authorized {
			progress = true
		}
		if isEmblemLoggedIn(page) && !isLoginFlowPage(page) {
			return nil
		}
		if !progress {
			time.Sleep(750 * time.Millisecond)
		}
	}
	return fmt.Errorf("[EmblemHealth] login flow did not complete")
}

func emblemPageState(page *rod.Page) string {
	res, err := page.Eval(`() => {
		const text = document.body ? document.body.innerText : '';
		const title = document.title || '';
		const buttons = Array.from(document.querySelectorAll('button, input[type="button"], input[type="submit"], a, [role="button"]'))
			.map(el => ((el.innerText || el.value || el.textContent || el.getAttribute('aria-label') || '') + '').replace(/\s+/g, ' ').trim())
			.filter(Boolean)
			.slice(0, 8);
		return JSON.stringify({
			url: location.href,
			title,
			finish: /Finish Logging In/i.test(text),
			mfa: /sent a code|Request Code|Two-Step Verification|Remember my Computer/i.test(text),
			remoteAuth: /Remote Access Authorization|Allow Access|Authorize Access/i.test(text) || location.href.includes('/setup/secur/RemoteAccessAuthorizationPage'),
			password: !!document.querySelector('#password, input[type="password"]'),
			buttons
		});
	}`)
	if err != nil {
		return fmt.Sprintf("stateErr=%v", err)
	}
	return res.Value.Str()
}

func isLoginFlowPage(page *rod.Page) bool {
	res, err := page.Eval(`() => {
		const url = String(location.href || '');
		const text = document.body ? document.body.innerText : '';
		return url.includes('/loginflow/') ||
			url.includes('/setup/secur/RemoteAccessAuthorizationPage') ||
			/Finish Logging In|Request Code|Two-Step Verification|Email Address|Remote Access Authorization|Allow Access/i.test(text);
	}`)
	return err == nil && res.Value.Bool()
}

func finishLoginFlowIfPresent(page *rod.Page) (bool, error) {
	found, err := clickButtonByText(page, `finish logging in`)
	if err != nil {
		return false, fmt.Errorf("[EmblemHealth] click Finish Logging In: %w", err)
	}
	if !found {
		return false, nil
	}
	log.Printf("[EmblemHealth] finishing Salesforce login flow")
	waitForEmblemPageSettle(page)
	waitForLoginTransition(page, 10*time.Second)
	return true, nil
}

func waitForLoginTransition(page *rod.Page, timeout time.Duration) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		res, err := page.Eval(`() => {
			const url = String(location.href || '');
			const text = document.body ? document.body.innerText : '';
			return JSON.stringify({
				mfa: /sent a code|Request Code|Two-Step Verification|Remember my Computer/i.test(text),
				remoteAuth: /Remote Access Authorization|Allow Access|Authorize Access/i.test(text) ||
					url.includes('/setup/secur/RemoteAccessAuthorizationPage'),
				loggedIn: url.includes('/ehprovider/s/') && !url.includes('/loginflow/'),
				password: !!document.querySelector('#password, input[type="password"]')
			});
		}`)
		if err == nil {
			var state struct {
				MFA        bool `json:"mfa"`
				RemoteAuth bool `json:"remoteAuth"`
				LoggedIn   bool `json:"loggedIn"`
				Password   bool `json:"password"`
			}
			if json.Unmarshal([]byte(res.Value.Str()), &state) == nil &&
				(state.MFA || state.RemoteAuth || state.LoggedIn || state.Password) {
				log.Printf("[EmblemHealth] login transition settled: %s", emblemPageState(page))
				return
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	log.Printf("[EmblemHealth] login transition wait expired: %s", emblemPageState(page))
}

func authorizeRemoteAccessIfPresent(page *rod.Page) (bool, error) {
	present, err := page.Eval(`() => {
		const url = String(location.href || '');
		const text = document.body ? document.body.innerText : '';
		return url.includes('/setup/secur/RemoteAccessAuthorizationPage') ||
			/Remote Access Authorization|Allow Access|Authorize Access/i.test(text);
	}`)
	if err != nil {
		return false, fmt.Errorf("[EmblemHealth] inspect remote access authorization: %w", err)
	}
	if !present.Value.Bool() {
		return false, nil
	}
	for _, label := range []string{"allow", "authorize", "approve", "continue"} {
		clicked, err := clickButtonByText(page, label)
		if err != nil {
			return false, fmt.Errorf("[EmblemHealth] authorize remote access: %w", err)
		}
		if clicked {
			log.Printf("[EmblemHealth] Salesforce remote access authorized")
			waitForEmblemPageSettle(page)
			return true, nil
		}
	}
	return false, fmt.Errorf("[EmblemHealth] remote access authorization page found but allow button was not found")
}

func clickButtonByText(page *rod.Page, text string) (bool, error) {
	res, err := page.Eval(`(needle) => {
		const normalize = (value) => String(value || '').replace(/\s+/g, ' ').trim().toLowerCase();
		needle = normalize(needle);
		const actionableSelector = 'button, input[type="button"], input[type="submit"], a, [role="button"]';
		const labelFor = (el) => normalize([
			el.innerText,
			el.value,
			el.textContent,
			el.getAttribute && el.getAttribute('aria-label'),
			el.getAttribute && el.getAttribute('title'),
			el.getAttribute && el.getAttribute('href')
		].filter(Boolean).join(' '));
		const click = (el) => {
			if (!el || el.disabled) return false;
			el.scrollIntoView({block: 'center', inline: 'center'});
			el.focus && el.focus();
			el.dispatchEvent(new MouseEvent('mousedown', {bubbles: true, cancelable: true, view: window}));
			el.dispatchEvent(new MouseEvent('mouseup', {bubbles: true, cancelable: true, view: window}));
			el.dispatchEvent(new MouseEvent('click', {bubbles: true, cancelable: true, view: window}));
			if (typeof el.click === 'function') el.click();
			return true;
		};
		for (const el of Array.from(document.querySelectorAll(actionableSelector))) {
			if (labelFor(el).includes(needle)) return click(el);
		}
		for (const el of Array.from(document.querySelectorAll('body *'))) {
			if (!labelFor(el).includes(needle)) continue;
			const action = el.closest(actionableSelector) || el;
			if (click(action)) return true;
		}
		return false;
	}`, text)
	if err != nil {
		return false, err
	}
	return res.Value.Bool(), nil
}

func completeEmailMFAIfPresent(page *rod.Page, input payers.SessionInput) (bool, error) {
	requestBtn, err := page.Timeout(2 * time.Second).Element(`#TwoStepVerification_nextBtn`)
	if err != nil {
		requestBtn, err = page.Timeout(2*time.Second).ElementR(`button, input[type="button"], input[type="submit"]`, `(?i)request code`)
	}
	codeAlreadySent := emblemCodeInputsVisible(page)
	if err != nil && !codeAlreadySent {
		return false, nil
	}
	if input.EmailMFA == nil {
		return false, fmt.Errorf("[EmblemHealth] email MFA requested but mailbox config is missing")
	}
	emailMFA := *input.EmailMFA
	if emailMFA.TimeoutMS < 150000 {
		emailMFA.TimeoutMS = 150000
	}
	codeRequestedAt := time.Now()
	if requestBtn != nil {
		log.Printf("[EmblemHealth] requesting email MFA code")
		if err := requestBtn.Click(proto.InputMouseButtonLeft, 1); err != nil {
			return false, fmt.Errorf("[EmblemHealth] click MFA request code: %w", err)
		}
	} else {
		log.Printf("[EmblemHealth] email MFA code already requested")
		codeRequestedAt = time.Now().Add(-2 * time.Minute)
	}
	code, err := mfa.GetEmailCode(emailMFA, codeRequestedAt)
	if err != nil {
		return false, fmt.Errorf("[EmblemHealth] get email MFA code: %w", err)
	}
	if filled, err := fillSplitMFACode(page, code); err != nil {
		return false, err
	} else if filled {
		return submitMFA(page)
	}
	codeEl, err := firstElementWithTimeout(page, []string{
		`input[autocomplete="one-time-code"]`,
		`input[inputmode="numeric"]`,
		`input[name*="otp" i]`,
		`input[id*="otp" i]`,
		`input[name*="code" i]`,
		`input[id*="code" i]`,
		`input[type="text"]`,
	}, 20*time.Second)
	if err != nil {
		return false, fmt.Errorf("[EmblemHealth] MFA code field not found: %w", err)
	}
	if err := codeEl.Input(code); err != nil {
		return false, fmt.Errorf("[EmblemHealth] fill MFA code: %w", err)
	}
	return submitMFA(page)
}

func emblemCodeInputsVisible(page *rod.Page) bool {
	res, err := page.Eval(`() => {
		const text = document.body ? document.body.innerText : '';
		const inputs = Array.from(document.querySelectorAll('input')).filter(el => {
			const type = (el.getAttribute('type') || 'text').toLowerCase();
			return type === 'text' || type === 'tel' || type === 'number' || type === '';
		});
		return /sent a code|code will expire|remember my computer/i.test(text) && inputs.length >= 6;
	}`)
	return err == nil && res.Value.Bool()
}

func fillSplitMFACode(page *rod.Page, code string) (bool, error) {
	code = strings.TrimSpace(code)
	if len(code) < 6 {
		return false, nil
	}
	res, err := page.Eval(`(code) => {
		const digits = String(code).replace(/\D/g, '').split('');
		const setNativeValue = (el, value) => {
			const proto = Object.getPrototypeOf(el);
			const descriptor = Object.getOwnPropertyDescriptor(proto, 'value') ||
				Object.getOwnPropertyDescriptor(HTMLInputElement.prototype, 'value');
			if (descriptor && descriptor.set) {
				descriptor.set.call(el, value);
			} else {
				el.value = value;
			}
		};
		const inputs = Array.from(document.querySelectorAll('input')).filter(el => {
			const type = (el.getAttribute('type') || 'text').toLowerCase();
			return (type === 'text' || type === 'tel' || type === 'number' || type === '') &&
				!el.disabled && el.offsetParent !== null;
		}).slice(0, digits.length);
		if (inputs.length < digits.length) return false;
		inputs.forEach((el, i) => {
			el.click();
			el.focus();
			el.dispatchEvent(new KeyboardEvent('keydown', { bubbles: true, key: digits[i] }));
			el.dispatchEvent(new KeyboardEvent('keypress', { bubbles: true, key: digits[i] }));
			setNativeValue(el, digits[i]);
			el.dispatchEvent(new Event('input', { bubbles: true }));
			el.dispatchEvent(new KeyboardEvent('keyup', { bubbles: true, key: digits[i] }));
			el.dispatchEvent(new Event('change', { bubbles: true }));
		});
		const last = inputs[digits.length - 1];
		last.blur();
		last.dispatchEvent(new Event('blur', { bubbles: true }));
		document.body && document.body.click();
		return true;
	}`, code)
	if err != nil {
		return false, fmt.Errorf("[EmblemHealth] fill split MFA code: %w", err)
	}
	if res.Value.Bool() {
		log.Printf("[EmblemHealth] split MFA code filled")
		time.Sleep(3 * time.Second)
		return true, nil
	}
	return false, nil
}

func submitMFA(page *rod.Page) (bool, error) {
	if _, err := firstElementWithTimeout(page, []string{
		`#RemMyComputer`,
		`input[type="checkbox"][name*="remember" i]`,
		`input[type="checkbox"][id*="remember" i]`,
	}, 5000*time.Millisecond); err == nil {
		// Set via AngularJS scope so ng-model is updated, then also native click as fallback
		res, jsErr := page.Eval(`() => {
			const el = document.getElementById('RemMyComputer');
			if (!el) return 'not found';
			try {
				const scope = angular.element(el).scope();
				scope.control.response = true;
				scope.$apply();
				return 'angular scope set';
			} catch(e) {
				if (!el.checked) el.click();
				return 'native click fallback';
			}
		}`)
		if jsErr == nil {
			log.Printf("[EmblemHealth] remember-my-computer: %s", res.Value.Str())
		} else {
			log.Printf("[EmblemHealth] remember-my-computer JS failed: %v", jsErr)
		}
		_ = acknowledgeRememberDisclaimer(page)
	} else {
		log.Printf("[EmblemHealth] remember-my-computer checkbox not found: %v", err)
	}
	clicked, err := clickMFASubmitWhenReady(page, 15*time.Second)
	if !clicked {
		return false, fmt.Errorf("[EmblemHealth] MFA submit button not found")
	}
	if err != nil {
		return false, fmt.Errorf("[EmblemHealth] submit MFA code: %w", err)
	}
	waitForEmblemPageSettle(page)
	log.Printf("[EmblemHealth] email MFA submitted")
	return true, nil
}

func acknowledgeRememberDisclaimer(page *rod.Page) error {
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		ok, err := clickFirstMatching(page, []string{`#disclaimerModal`})
		if err != nil {
			return fmt.Errorf("[EmblemHealth] acknowledge remember disclaimer: %w", err)
		}
		if ok {
			log.Printf("[EmblemHealth] remember-computer disclaimer acknowledged")
			return nil
		}
		time.Sleep(2000 * time.Millisecond)
	}
	return nil
}

func clickMFASubmitWhenReady(page *rod.Page, timeout time.Duration) (bool, error) {
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		for _, label := range []string{"next", "verify", "submit", "continue"} {
			clicked, err := clickButtonByText(page, label)
			if err != nil {
				lastErr = err
				continue
			}
			if clicked {
				return true, nil
			}
		}
		clicked, err := clickFirstMatching(page, []string{
			`button[type="submit"]`,
			`button[id*="verify" i]`,
		})
		if err != nil {
			lastErr = err
		}
		if clicked {
			return true, nil
		}
		time.Sleep(250 * time.Millisecond)
	}
	return false, lastErr
}

func clickFirstMatching(page *rod.Page, selectors []string) (bool, error) {
	data, err := json.Marshal(selectors)
	if err != nil {
		return false, err
	}
	res, err := page.Eval(`(selectorsJSON) => {
		const selectors = JSON.parse(selectorsJSON);
		for (const selector of selectors) {
			const el = document.querySelector(selector);
			if (!el || el.disabled || el.offsetParent === null) continue;
			el.scrollIntoView({block: 'center', inline: 'center'});
			el.click();
			return true;
		}
		return false;
	}`, string(data))
	if err != nil {
		return false, err
	}
	return res.Value.Bool(), nil
}

func firstElement(page *rod.Page, selectors []string) (*rod.Element, error) {
	return firstElementWithTimeout(page, selectors, 1500*time.Millisecond)
}

func firstElementWithTimeout(page *rod.Page, selectors []string, timeout time.Duration) (*rod.Element, error) {
	var lastErr error
	for _, selector := range selectors {
		el, err := page.Timeout(timeout).Element(selector)
		if err == nil {
			return el, nil
		}
		lastErr = err
	}
	return nil, lastErr
}

func captureApexCtxViaFormSubmit(session *agentbrowser.Session, warmupMemberID string, timeout time.Duration) (emblemApexCtx, []*proto.NetworkCookie, error) {
	ctxCh := make(chan emblemApexCtx, 1)
	router := session.Browser.HijackRequests()
	logCount := 0
	router.MustAdd("*provider.emblemhealth.com/*", func(h *rod.Hijack) {
		reqURL := h.Request.URL().String()
		method := h.Request.Method()
		body := h.Request.Body()
		if method == http.MethodPost && (strings.Contains(reqURL, "/ehprovider/apexremote") || strings.Contains(body, "CardCanvasController") || strings.Contains(body, "IntegrationProcedureService")) {
			logCount++
			log.Printf("[EmblemHealth] warmup network request #%d method=%s url=%s bodyLen=%d summary=%s",
				logCount, method, reqURL, len(body), summarizeApexRemoteBody(body))
		}
		if strings.Contains(reqURL, "/ehprovider/apexremote") {
			var req struct {
				Action string        `json:"action"`
				Method string        `json:"method"`
				Data   []string      `json:"data"`
				Ctx    emblemApexCtx `json:"ctx"`
			}
			if json.Unmarshal([]byte(body), &req) == nil {
				if req.Ctx.Authorization != "" {
					log.Printf("[EmblemHealth] warmup captured ctx action=%s method=%s data=%s vid=%s",
						req.Action, req.Method, apexDataMethod(req.Data), req.Ctx.VID)
					select {
					case ctxCh <- req.Ctx:
					default:
					}
				} else {
					log.Printf("[EmblemHealth] warmup apexremote without authorization ctx action=%s method=%s data=%s ctxLen=%d",
						req.Action, req.Method, apexDataMethod(req.Data), len(rawCtxFromApexBody(body)))
				}
			} else {
				log.Printf("[EmblemHealth] warmup apexremote body parse failed bodyPrefix=%q", safeBodyPrefix(body, 240))
			}
		}
		h.ContinueRequest(&proto.FetchContinueRequest{})
	})
	go router.Run()
	defer router.Stop()

	if err := installApexRemoteBrowserHook(session.Page); err != nil {
		log.Printf("[EmblemHealth] warmup browser hook install warning: %v", err)
	}
	if err := fillAndClickEligibilityForm(session.Page, warmupMemberID, 30*time.Second); err != nil {
		return emblemApexCtx{}, nil, err
	}
	log.Printf("[EmblemHealth] warmup form submitted memberId=%s", warmupMemberID)

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		select {
		case apexCtx := <-ctxCh:
			return apexCtxWithCookies(session, apexCtx)
		default:
		}
		if apexCtx, ok := capturedApexCtxFromBrowserHook(session.Page); ok {
			return apexCtxWithCookies(session, apexCtx)
		}
		time.Sleep(500 * time.Millisecond)
	}
	return emblemApexCtx{}, nil, fmt.Errorf("timeout waiting for apexremote ctx")
}

func apexCtxWithCookies(session *agentbrowser.Session, apexCtx emblemApexCtx) (emblemApexCtx, []*proto.NetworkCookie, error) {
	log.Printf("[EmblemHealth] captured apexremote ctx vid=%s", apexCtx.VID)
	cookieRes, cookieErr := proto.StorageGetCookies{}.Call(session.Browser)
	if cookieErr != nil {
		log.Printf("[EmblemHealth] get cookies warning: %v", cookieErr)
		return apexCtx, nil, nil
	}
	return apexCtx, cookieRes.Cookies, nil
}

// networkCapture captures apexremote responses at the CDP (Chrome DevTools Protocol) level.
// This works across all frames and origins — no JS hook limitations.
type networkCapture struct {
	mu       sync.Mutex
	requests map[string]string
	asyncIDs map[string]string
	pending  map[proto.NetworkRequestID]string // requestID → Vlocity IP method name
	results  map[string]string                 // method → raw response body
	stopped  bool
	stop     func()
}

// startDetailCapture begins CDP-level network monitoring on the page.
// Start it before any action that may trigger detail API calls.
func startDetailCapture(page *rod.Page) *networkCapture {
	c := &networkCapture{
		pending:  make(map[proto.NetworkRequestID]string),
		requests: make(map[string]string),
		asyncIDs: make(map[string]string),
		results:  make(map[string]string),
	}
	maxPost := 65536
	_ = proto.NetworkEnable{MaxPostDataSize: &maxPost}.Call(page)
	wait := page.EachEvent(
		func(e *proto.NetworkRequestWillBeSent) bool {
			c.mu.Lock()
			stopped := c.stopped
			c.mu.Unlock()
			if stopped {
				return true
			}
			if !strings.Contains(e.Request.URL, "/ehprovider/apexremote") {
				return false
			}
			method := vlocityMethodFromPostData(e.Request.PostData)
			if method != "Member_DetailsInformation" && method != "MemberDetails_DentalInNetwork" {
				c.mu.Lock()
				method = c.asyncMethodFromPostDataMu(e.Request.PostData)
				c.mu.Unlock()
				if method != "Member_DetailsInformation" && method != "MemberDetails_DentalInNetwork" {
					return false
				}
			}
			if method == "Member_DetailsInformation" && !memberDetailsRequestHasEncryptedArgs(e.Request.PostData) {
				log.Printf("[EmblemHealth] CDP skipped blank Member_DetailsInformation request")
				return false
			}
			c.mu.Lock()
			c.pending[e.RequestID] = method
			c.requests[method] = e.Request.PostData
			c.mu.Unlock()
			return false
		},
		func(e *proto.NetworkLoadingFinished) bool {
			c.mu.Lock()
			if c.stopped {
				c.mu.Unlock()
				return true
			}
			method, ok := c.pending[e.RequestID]
			if ok {
				delete(c.pending, e.RequestID)
			}
			c.mu.Unlock()
			if !ok {
				return false
			}
			resp, err := proto.NetworkGetResponseBody{RequestID: e.RequestID}.Call(page)
			if err != nil {
				log.Printf("[EmblemHealth] CDP getResponseBody method=%s: %v", method, err)
				return false
			}
			text := resp.Body
			if resp.Base64Encoded {
				if dec, decErr := base64.StdEncoding.DecodeString(text); decErr == nil {
					text = string(dec)
				}
			}
			if responseID, ok := asyncWaitResponseID(text); ok {
				c.mu.Lock()
				c.asyncIDs[responseID] = method
				c.mu.Unlock()
				log.Printf("[EmblemHealth] CDP captured async WAIT method=%s responseId=%s", method, responseID)
				return false
			}
			c.mu.Lock()
			c.results[method] = text
			c.mu.Unlock()
			log.Printf("[EmblemHealth] CDP captured method=%s bodyLen=%d", method, len(text))
			return false
		},
	)
	done := make(chan struct{})
	go func() {
		defer close(done)
		wait()
	}()
	c.stop = func() {
		c.mu.Lock()
		c.stopped = true
		c.mu.Unlock()
	}
	return c
}

// countTargets returns how many of the target detailMethods are in c.results.
// Must be called with c.mu held.
func (c *networkCapture) countTargetsMu() int {
	n := 0
	for _, m := range detailMethods {
		if _, ok := c.results[m]; ok {
			n++
		}
	}
	return n
}

func (c *networkCapture) asyncMethodFromPostDataMu(postData string) string {
	for responseID, method := range c.asyncIDs {
		if responseID != "" && strings.Contains(postData, responseID) {
			return method
		}
	}
	return ""
}

func (c *networkCapture) requestBodyForMethod(method string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.requests[method]
}

func (c *networkCapture) hasResult(method string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, ok := c.results[method]
	return ok
}

// waitForRequestBody blocks until the named method's request body is captured.
// For Emblem detail fallback this is enough because encrypted arguments live in the request.
func (c *networkCapture) waitForRequestBody(method string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		c.mu.Lock()
		body := c.requests[method]
		c.mu.Unlock()
		if strings.TrimSpace(body) != "" {
			return true
		}
		time.Sleep(250 * time.Millisecond)
	}
	log.Printf("[EmblemHealth] timed out waiting for CDP request method=%s after %s", method, timeout)
	return false
}

// waitForMethod blocks until the named method appears in results or timeout elapses.
// Returns true if the method was captured before the deadline.
func (c *networkCapture) waitForMethod(method string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		c.mu.Lock()
		_, ok := c.results[method]
		c.mu.Unlock()
		if ok {
			return true
		}
		time.Sleep(500 * time.Millisecond)
	}
	log.Printf("[EmblemHealth] timed out waiting for CDP method=%s after %s", method, timeout)
	return false
}

// capturedDetailData returns whatever target detail methods have been captured so far.
func (c *networkCapture) capturedDetailData() map[string]json.RawMessage {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make(map[string]json.RawMessage)
	for _, m := range detailMethods {
		if body, ok := c.results[m]; ok {
			out[m] = json.RawMessage(body)
		}
	}
	return out
}

// waitForDetailMethods polls until all target detail methods are captured (or timeout),
// then returns whatever was collected. The caller owns stopping the listener.
func (c *networkCapture) waitForDetailMethods(timeout time.Duration) map[string]json.RawMessage {
	deadline := time.Now().Add(timeout)
	nextLog := time.Now()
	for time.Now().Before(deadline) {
		c.mu.Lock()
		found := c.countTargetsMu()
		c.mu.Unlock()
		if found >= len(detailMethods) {
			log.Printf("[EmblemHealth] all %d detail responses captured via CDP", found)
			break
		}
		if time.Now().After(nextLog) {
			log.Printf("[EmblemHealth] waiting for detail responses (CDP): %d/%d", found, len(detailMethods))
			nextLog = time.Now().Add(5 * time.Second)
		}
		time.Sleep(500 * time.Millisecond)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make(map[string]json.RawMessage, len(c.results))
	for _, method := range detailMethods {
		if body, ok := c.results[method]; ok {
			out[method] = json.RawMessage(body)
		}
	}
	if len(out) > 0 && len(out) < len(detailMethods) {
		log.Printf("[EmblemHealth] CDP detail responses partial: %d/%d", len(out), len(detailMethods))
	}
	return out
}

func vlocityMethodFromPostData(postData string) string {
	if postData == "" {
		return ""
	}
	var req struct {
		Data []string `json:"data"`
	}
	if err := json.Unmarshal([]byte(postData), &req); err != nil || len(req.Data) < 2 {
		return ""
	}
	return req.Data[1]
}

func memberDetailsRequestHasEncryptedArgs(postData string) bool {
	var req struct {
		Data []string `json:"data"`
	}
	if err := json.Unmarshal([]byte(postData), &req); err != nil || len(req.Data) < 3 {
		return false
	}
	var payload map[string]string
	if err := json.Unmarshal([]byte(req.Data[2]), &payload); err != nil {
		return false
	}
	return strings.TrimSpace(payload["memberId"]) != "" &&
		strings.TrimSpace(payload["planCode"]) != "" &&
		strings.TrimSpace(payload["eligibilityEffectiveDate"]) != "" &&
		strings.TrimSpace(payload["eligibilityTerminationDate"]) != ""
}

func invokeMissingDetailMethodsFromCapturedDetail(page *rod.Page, detailRequestBody, memberID string, existing map[string]json.RawMessage) (map[string]json.RawMessage, error) {
	detailRequestBody = strings.TrimSpace(detailRequestBody)
	if detailRequestBody == "" {
		return nil, fmt.Errorf("missing captured Member_DetailsInformation request")
	}
	base, params, err := parseCapturedDetailRequest(detailRequestBody)
	if err != nil {
		return nil, err
	}
	encryptedMemberID := strings.TrimSpace(params["memberId"])
	encryptedPlanCode := strings.TrimSpace(params["planCode"])
	encryptedEffectiveDate := strings.TrimSpace(params["eligibilityEffectiveDate"])
	encryptedTerminationDate := strings.TrimSpace(params["eligibilityTerminationDate"])
	if encryptedMemberID == "" || encryptedPlanCode == "" || encryptedEffectiveDate == "" || encryptedTerminationDate == "" {
		return nil, fmt.Errorf("captured detail request missing encrypted member/plan/date fields")
	}

	type detailCall struct {
		method         string
		remotingMethod string
		payload        map[string]any
	}
	calls := []detailCall{
		{
			method:         "MemberDetails_ToothHistory",
			remotingMethod: "doGenericInvoke",
			payload:        map[string]any{"memberId": memberID},
		},
		{
			method:         "MemberDetails_DentalOutNetwork",
			remotingMethod: "doGenericInvoke",
			payload: map[string]any{
				"memberEligibilityFromDate": encryptedEffectiveDate,
				"memberEligibilityToDate":   encryptedTerminationDate,
				"memberId":                  memberID,
				"planId":                    encryptedPlanCode,
			},
		},
		{
			method:         "MemberDetails_BenefitAccumulator",
			remotingMethod: "doGenericInvoke",
			payload: map[string]any{
				"accumulatorType": "Annual Maximum",
				"effectiveDate":   encryptedEffectiveDate,
				"memberId":        memberID,
				"planCode":        encryptedPlanCode,
				"terminationDate": encryptedTerminationDate,
			},
		},
		{
			method:         "MemberDetails_DentalLimitation",
			remotingMethod: "doGenericInvoke",
			payload: map[string]any{
				"memberEligibilityFromDate": encryptedEffectiveDate,
				"memberEligibilityToDate":   encryptedTerminationDate,
				"memberId":                  memberID,
				"planId":                    encryptedPlanCode,
			},
		},
		{
			method:         "AdditionalInsurance_Information",
			remotingMethod: "doGenericInvoke",
			payload:        map[string]any{"memberId": encryptedMemberID},
		},
	}

	out := map[string]json.RawMessage{}
	for i, call := range calls {
		if _, ok := existing[call.method]; ok {
			continue
		}
		body, err := buildDetailApexRequest(base, call.remotingMethod, call.method, call.payload, i+30)
		if err != nil {
			log.Printf("[EmblemHealth] build detail fallback method=%s: %v", call.method, err)
			continue
		}
		raw, err := invokeApexRemoteRawInBrowser(page, body)
		if err != nil {
			log.Printf("[EmblemHealth] invoke detail fallback method=%s: %v", call.method, err)
			continue
		}
		out[call.method] = json.RawMessage(raw)
		statusCode, resultLen, apexErr := apexResponseSummary(raw)
		log.Printf("[EmblemHealth] detail fallback result method=%s statusCode=%d resultBytes=%d apexError=%q bodyLen=%d",
			call.method, statusCode, resultLen, apexErr, len(raw))
		time.Sleep(750 * time.Millisecond)
	}
	return out, nil
}

func capturedMethodNames(data map[string]json.RawMessage) []string {
	names := make([]string, 0, len(data))
	for _, method := range detailMethods {
		if _, ok := data[method]; ok {
			names = append(names, method)
		}
	}
	return names
}

func apexResponseSummary(raw string) (int, int, string) {
	var responses []struct {
		StatusCode int    `json:"statusCode"`
		Result     string `json:"result"`
		Message    string `json:"message"`
	}
	if err := json.Unmarshal([]byte(raw), &responses); err != nil || len(responses) == 0 {
		return 0, 0, "decode apex response summary failed"
	}
	message := strings.TrimSpace(responses[0].Message)
	if message == "" && responses[0].StatusCode >= 200 && responses[0].StatusCode < 300 {
		var result struct {
			ErrorCode string `json:"errorCode"`
			Error     string `json:"error"`
		}
		if err := json.Unmarshal([]byte(responses[0].Result), &result); err == nil && !strings.EqualFold(strings.TrimSpace(result.Error), "OK") {
			message = strings.TrimSpace(result.Error)
		}
	}
	return responses[0].StatusCode, len(responses[0].Result), message
}

func asyncWaitResponseID(raw string) (string, bool) {
	var responses []struct {
		Result json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal([]byte(raw), &responses); err != nil || len(responses) == 0 || len(responses[0].Result) == 0 {
		return "", false
	}
	if responseID, ok := asyncWaitResponseIDFromResult(responses[0].Result); ok {
		return responseID, true
	}
	var resultString string
	if err := json.Unmarshal(responses[0].Result, &resultString); err == nil {
		return asyncWaitResponseIDFromResult(json.RawMessage(resultString))
	}
	return "", false
}

func asyncWaitResponseIDFromResult(raw json.RawMessage) (string, bool) {
	var result struct {
		Result     string `json:"result"`
		ResponseID string `json:"responseId"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return "", false
	}
	if strings.EqualFold(strings.TrimSpace(result.Result), "WAIT") && strings.TrimSpace(result.ResponseID) != "" {
		return strings.TrimSpace(result.ResponseID), true
	}
	return "", false
}

type capturedDetailRequest struct {
	Action  string
	Ctx     json.RawMessage
	Service string
	Options string
}

func parseCapturedDetailRequest(body string) (capturedDetailRequest, map[string]string, error) {
	var req struct {
		Action string            `json:"action"`
		Ctx    json.RawMessage   `json:"ctx"`
		Data   []json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		return capturedDetailRequest{}, nil, fmt.Errorf("decode captured detail request: %w", err)
	}
	if len(req.Data) < 4 || len(req.Ctx) == 0 {
		return capturedDetailRequest{}, nil, fmt.Errorf("captured detail request missing ctx/data")
	}
	service := rawJSONString(req.Data[0])
	payload := rawJSONString(req.Data[2])
	options := rawJSONString(req.Data[3])
	var params map[string]string
	if err := json.Unmarshal([]byte(payload), &params); err != nil {
		return capturedDetailRequest{}, nil, fmt.Errorf("decode captured detail payload: %w", err)
	}
	action := strings.TrimSpace(req.Action)
	if action == "" {
		action = "vlocity_ins.CardCanvasController"
	}
	if dot := strings.Index(action, ".do"); dot > 0 {
		action = action[:dot]
	}
	return capturedDetailRequest{
		Action:  action,
		Ctx:     req.Ctx,
		Service: firstNonEmptyString(service, "vlocity_ins.IntegrationProcedureService"),
		Options: firstNonEmptyString(options, `{"vlcClass":"vlocity_ins.IntegrationProcedureService"}`),
	}, params, nil
}

func dentalDetailURLFromCapturedRequest(detailRequestBody string) (string, error) {
	_, params, err := parseCapturedDetailRequest(detailRequestBody)
	if err != nil {
		return "", err
	}
	encryptedMemberID := strings.TrimSpace(params["memberId"])
	encryptedPlanCode := strings.TrimSpace(params["planCode"])
	encryptedEffectiveDate := strings.TrimSpace(params["eligibilityEffectiveDate"])
	encryptedTerminationDate := strings.TrimSpace(params["eligibilityTerminationDate"])
	if encryptedMemberID == "" || encryptedPlanCode == "" || encryptedEffectiveDate == "" || encryptedTerminationDate == "" {
		return "", fmt.Errorf("captured detail request missing encrypted member/plan/date fields")
	}
	u, err := url.Parse(providerBaseURL + "/ehprovider/s/dental-member-eligibility-details")
	if err != nil {
		return "", err
	}
	q := u.Query()
	q.Set("memberId", encryptedMemberID)
	q.Set("planCode", encryptedPlanCode)
	q.Set("status", firstNonEmptyString(strings.TrimSpace(params["status"]), "Active"))
	q.Set("effDate", encryptedEffectiveDate)
	q.Set("termDate", encryptedTerminationDate)
	q.Set("showPCP", firstNonEmptyString(strings.TrimSpace(params["showPCP"]), "true"))
	q.Set("tenantId", "EMBLEM")
	q.Set("planType", "Dental")
	q.Set("isEvicore", "false")
	u.RawQuery = q.Encode()
	return u.String(), nil
}

func buildDetailApexRequest(base capturedDetailRequest, remotingMethod, ipMethod string, payload map[string]any, tid int) ([]byte, error) {
	payloadJSON, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	data := []any{
		base.Service,
		ipMethod,
		string(payloadJSON),
		base.Options,
	}
	if remotingMethod == "doAsyncInvoke" {
		data = append(data, nil)
	}
	reqBody := map[string]any{
		"action": base.Action,
		"method": remotingMethod,
		"ctx":    base.Ctx,
		"data":   data,
		"type":   "rpc",
		"tid":    tid,
	}
	return json.Marshal(reqBody)
}

func invokeApexRemoteRawInBrowser(page *rod.Page, body []byte) (string, error) {
	res, err := page.Timeout(60*time.Second).Eval(`async (url, body) => {
		const response = await fetch(url, {
			method: 'POST',
			credentials: 'include',
			headers: {
				'accept': '*/*',
				'content-type': 'application/json',
				'x-requested-with': 'XMLHttpRequest',
				'x-user-agent': 'Visualforce-Remoting'
			},
			body
		});
		const text = await response.text();
		return JSON.stringify({ status: response.status, text });
	}`, apexRemoteURL, string(body))
	if err != nil {
		return "", err
	}
	var envelope struct {
		Status int    `json:"status"`
		Text   string `json:"text"`
	}
	if err := json.Unmarshal([]byte(res.Value.Str()), &envelope); err != nil {
		return "", fmt.Errorf("decode browser detail envelope: %w", err)
	}
	if envelope.Status != http.StatusOK {
		return "", fmt.Errorf("browser detail HTTP %d: %.200s", envelope.Status, envelope.Text)
	}
	return envelope.Text, nil
}

func rawJSONString(raw json.RawMessage) string {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return strings.TrimSpace(s)
	}
	return ""
}

func firstNonEmptyString(values ...string) string {
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return ""
}

// apexResponseHookJS returns the self-contained JS that patches XHR and fetch on
// the current window. Safe to run via EvalOnNewDocument (no DOM access needed).
func apexResponseHookJS() string {
	return `(function() {
		if (window.__emblemRespHookInstalled) return;
		window.__emblemRespHookInstalled = true;
		window.__emblemApexResponses = window.__emblemApexResponses || [];
		const isApex = (url) => String(url || '').includes('/ehprovider/apexremote');
		const extractMethod = (body) => {
			try {
				const p = JSON.parse(typeof body === 'string' ? body : String(body || ''));
				return (p.data && p.data[1]) || '';
			} catch(e) { return ''; }
		};
		if (window.XMLHttpRequest) {
			const proto = window.XMLHttpRequest.prototype;
			const origOpen = proto.open;
			const origSend = proto.send;
			proto.open = function(m, url) { this.__aurl = url; return origOpen.apply(this, arguments); };
			proto.send = function(body) {
				if (isApex(this.__aurl)) {
					const method = extractMethod(body);
					this.addEventListener('load', () => {
						window.__emblemApexResponses.push({method: method, text: this.responseText});
					});
				}
				return origSend.apply(this, arguments);
			};
		}
		if (window.fetch) {
			const origFetch = window.fetch;
			window.fetch = function(input, init) {
				const url = typeof input === 'string' ? input : (input && input.url) || '';
				const p = origFetch.apply(this, arguments);
				if (isApex(url)) {
					const method = extractMethod(init && init.body);
					p.then(r => r.clone().text().then(t => window.__emblemApexResponses.push({method: method, text: t})));
				}
				return p;
			};
		}
	})()`
}

// installApexRemoteResponseHook patches XHR and fetch in the main frame and all
// same-origin iframes so that responses to /ehprovider/apexremote are stored in
// window.__emblemApexResponses. Call clearCapturedApexResponse before each form
// submission and waitForCapturedApexResponse after to retrieve the result.
func installApexRemoteResponseHook(page *rod.Page) error {
	_, err := page.Eval(`() => {
		const install = (win) => {
			if (!win || win.__emblemRespHookInstalled) return false;
			win.__emblemRespHookInstalled = true;
			win.__emblemApexResponses = [];
			const isApex = (url) => String(url || '').includes('/ehprovider/apexremote');
			const extractMethod = (body) => {
				try {
					const p = JSON.parse(typeof body === 'string' ? body : String(body || ''));
					return (p.data && p.data[1]) || '';
				} catch(e) { return ''; }
			};
			if (win.XMLHttpRequest) {
				const proto = win.XMLHttpRequest.prototype;
				const origOpen = proto.open;
				const origSend = proto.send;
				proto.open = function(m, url) { this.__aurl = url; return origOpen.apply(this, arguments); };
				proto.send = function(body) {
					if (isApex(this.__aurl)) {
						const method = extractMethod(body);
						this.addEventListener('load', () => {
							win.__emblemApexResponses.push({method: method, text: this.responseText});
						});
					}
					return origSend.apply(this, arguments);
				};
			}
			if (win.fetch) {
				const origFetch = win.fetch;
				win.fetch = function(input, init) {
					const url = typeof input === 'string' ? input : (input && input.url) || '';
					const p = origFetch.apply(this, arguments);
					if (isApex(url)) {
						const method = extractMethod(init && init.body);
						p.then(r => r.clone().text().then(t => win.__emblemApexResponses.push({method: method, text: t})));
					}
					return p;
				};
			}
			return true;
		};
		let n = 0;
		if (install(window)) n++;
		for (const f of document.querySelectorAll('iframe')) {
			try { if (install(f.contentWindow)) n++; } catch(e) {}
		}
		return n;
	}`)
	if err == nil {
	}
	return err
}

func clearCapturedApexResponse(page *rod.Page) {
	_, _ = page.Eval(`() => {
		const contexts = [window];
		for (const f of document.querySelectorAll('iframe')) {
			try { if (f.contentWindow) contexts.push(f.contentWindow); } catch(e) {}
		}
		for (const win of contexts) { win.__emblemApexResponses = []; }
	}`)
}

func waitForCapturedApexResponse(page *rod.Page, timeout time.Duration) (string, error) {
	deadline := time.Now().Add(timeout)
	nextLog := time.Now()
	for time.Now().Before(deadline) {
		res, err := page.Eval(`() => {
			const contexts = [window];
			for (const f of document.querySelectorAll('iframe')) {
				try { if (f.contentWindow) contexts.push(f.contentWindow); } catch(e) {}
			}
			for (const win of contexts) {
				const r = win.__emblemApexResponses || [];
				if (r.length > 0) {
					const last = r[r.length - 1];
					if (typeof last === 'string') return last;
					if (last && last.text) return last.text;
				}
			}
			return '';
		}`)
		if err == nil {
			raw := strings.TrimSpace(res.Value.Str())
			if raw != "" {
				return raw, nil
			}
		}
		if time.Now().After(nextLog) {
			nextLog = time.Now().Add(5 * time.Second)
		}
		time.Sleep(500 * time.Millisecond)
	}
	return "", fmt.Errorf("timeout waiting for apexremote response after %s", timeout)
}

func resetEligibilityForm(page *rod.Page) {
	_, _ = page.Eval(`() => {
		const contexts = [{doc: document, win: window}];
		for (const f of document.querySelectorAll('iframe')) {
			try { if (f.contentDocument) contexts.push({doc: f.contentDocument, win: f.contentWindow}); } catch(e) {}
		}
		for (const {doc, win} of contexts) {
			const btn = doc.querySelector('#reset-search');
			if (!btn) continue;
			try { win.angular.element(btn).scope().resetSearch(); return true; } catch(e) {}
			btn.click();
			return true;
		}
		return false;
	}`)
	time.Sleep(1 * time.Second)
}

var detailMethods = []string{
	"Member_DetailsInformation",
	"MemberDetails_ToothHistory",
	"MemberDetails_DentalInNetwork",
	"MemberDetails_DentalOutNetwork",
	"MemberDetails_BenefitAccumulator",
	"MemberDetails_DentalLimitation",
	"AdditionalInsurance_Information",
}

// clickMemberResultRow waits for a result row containing memberID to appear in the
// DOM (the Vlocity framework renders it asynchronously after the API response), then
// clicks it to trigger the 6 detail API calls.
func clickMemberResultRow(page *rod.Page, memberID string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		res, err := page.Eval(`(memberId) => {
			const needle = String(memberId).toUpperCase().replace(/\s+/g, '');
			const contexts = [{doc: document, win: window}];
			for (const f of document.querySelectorAll('iframe')) {
				try { if (f.contentDocument) contexts.push({doc: f.contentDocument, win: f.contentWindow}); } catch(e) {}
			}
			const doClick = (el, win) => {
				if (!el) return false;
				el.scrollIntoView({block: 'center', inline: 'center'});
				el.dispatchEvent(new MouseEvent('mousedown', {bubbles: true, cancelable: true, view: win}));
				el.dispatchEvent(new MouseEvent('mouseup',  {bubbles: true, cancelable: true, view: win}));
				el.dispatchEvent(new MouseEvent('click',    {bubbles: true, cancelable: true, view: win}));
				if (typeof el.click === 'function') el.click();
				return true;
			};
			for (const {doc, win} of contexts) {
				// Scan table/grid rows, skipping the search form area.
				for (const row of Array.from(doc.querySelectorAll('tr, [role="row"]'))) {
					// Skip rows that contain the search input (form area, not results).
					if (row.querySelector('#bulkeligiblityMemberID, input[type="text"], input[type="search"]')) continue;
					const text = (row.innerText || row.textContent || '').replace(/\s+/g, '').toUpperCase();
					if (!text.includes(needle)) continue;
					const clickable = row.querySelector('a, button, [role="button"]') || row;
					return doClick(clickable, win);
				}
				// Fallback: a td/span/div whose trimmed text is exactly the member ID.
				for (const el of Array.from(doc.querySelectorAll('td, span, div'))) {
					const text = (el.innerText || el.textContent || '').trim().toUpperCase();
					if (text !== needle) continue;
					const row = el.closest('tr, [role="row"]') || el;
					if (row.querySelector('#bulkeligiblityMemberID, input[type="text"], input[type="search"]')) continue;
					return doClick(row, win);
				}
			}
			return false;
		}`, memberID)
		if err == nil && res.Value.Bool() {
			log.Printf("[EmblemHealth] clicked member result row memberId=%s", memberID)
			return nil
		}
		time.Sleep(1 * time.Second)
	}
	return fmt.Errorf("[EmblemHealth] member result row not found memberId=%s after %s", memberID, timeout)
}

func clickInNetworkAccordion(page *rod.Page, timeout time.Duration) (bool, error) {
	deadline := time.Now().Add(timeout)
	nextLog := time.Now()
	var lastState struct {
		Found      bool   `json:"found"`
		Clicked    bool   `json:"clicked"`
		Expanded   string `json:"expanded"`
		Candidates int    `json:"candidates"`
		Sections   string `json:"sections"`
		URL        string `json:"url"`
	}
	for time.Now().Before(deadline) {
		res, err := page.Eval(`() => {
			const contexts = [{doc: document, win: window}];
			for (const f of document.querySelectorAll('iframe')) {
				try { if (f.contentDocument) contexts.push({doc: f.contentDocument, win: f.contentWindow}); } catch(e) {}
			}
			const normalize = (value) => String(value || '').replace(/\s+/g, ' ').trim().toLowerCase();
			const deepElements = (root) => {
				const out = [];
				const visit = (node) => {
					if (!node) return;
					if (node.nodeType === Node.ELEMENT_NODE) {
						out.push(node);
						if (node.shadowRoot) visit(node.shadowRoot);
					}
					const kids = node.children || [];
					for (const child of kids) visit(child);
				};
				visit(root);
				return out;
			};
			const closestClickable = (el) => {
				let cur = el;
				for (let i = 0; cur && i < 10; i++, cur = cur.parentElement || (cur.getRootNode && cur.getRootNode().host)) {
					if (cur.matches && cur.matches('button, h3, .slds-section__title, .slds-section__title-action, [ng-click], [role="button"]')) return cur;
				}
				return el;
			};
			const click = (el, win) => {
				el.scrollIntoView({block: 'center', inline: 'center'});
				el.focus && el.focus();
				el.dispatchEvent(new MouseEvent('mousedown', {bubbles: true, cancelable: true, view: win}));
				el.dispatchEvent(new MouseEvent('mouseup', {bubbles: true, cancelable: true, view: win}));
				el.dispatchEvent(new MouseEvent('click', {bubbles: true, cancelable: true, view: win}));
				if (typeof el.click === 'function') el.click();
			};
			let candidates = 0;
			const sections = [];
			for (const {doc, win} of contexts) {
				const elements = deepElements(doc);
				candidates += elements.length;
				const direct = elements.find(el => el.getAttribute && el.getAttribute('aria-controls') === 'a7Q1I000000kBJdUAM');
				if (direct) {
					const expanded = direct.getAttribute('aria-expanded') || '';
					click(direct, win);
					return JSON.stringify({found: true, clicked: true, expanded, candidates, sections: sections.join('|'), url: location.href});
				}
				for (const el of elements) {
					const label = normalize([
						el.getAttribute && el.getAttribute('title'),
						el.innerText,
						el.textContent,
						el.getAttribute && el.getAttribute('aria-label')
					].filter(Boolean).join(' '));
					if ((el.matches && el.matches('.slds-section__title, .slds-section__title-action, h3, button')) || (el.getAttribute && el.getAttribute('title'))) {
						const sectionLabel = normalize([
							el.getAttribute && el.getAttribute('title'),
							el.innerText,
							el.textContent
						].filter(Boolean).join(' '));
						if (sectionLabel && sectionLabel.length < 80 && sections.length < 12 && !sections.includes(sectionLabel)) sections.push(sectionLabel);
					}
					if (!label.includes('in-network')) continue;
					const target = closestClickable(el);
					const expandedNode = target.matches && target.matches('[aria-expanded]') ? target : target.querySelector && target.querySelector('[aria-expanded]');
					const expanded = expandedNode ? expandedNode.getAttribute('aria-expanded') : '';
					click(target, win);
					return JSON.stringify({found: true, clicked: true, expanded, candidates, sections: sections.join('|'), url: location.href});
				}
			}
			return JSON.stringify({found: false, clicked: false, expanded: '', candidates, sections: sections.join('|'), url: location.href});
		}`)
		if err != nil {
			return false, fmt.Errorf("[EmblemHealth] click In-network accordion eval: %w", err)
		}
		if err := json.Unmarshal([]byte(res.Value.Str()), &lastState); err != nil {
			return false, fmt.Errorf("[EmblemHealth] parse In-network accordion state: %w", err)
		}
		if lastState.Found && lastState.Clicked {
			log.Printf("[EmblemHealth] In-network accordion state found=%t clicked=%t expanded=%q candidates=%d sections=%q", lastState.Found, lastState.Clicked, lastState.Expanded, lastState.Candidates, lastState.Sections)
			return true, nil
		}
		if time.Now().After(nextLog) {
			log.Printf("[EmblemHealth] waiting for In-network accordion found=%t candidates=%d sections=%q url=%s", lastState.Found, lastState.Candidates, lastState.Sections, lastState.URL)
			nextLog = time.Now().Add(5 * time.Second)
		}
		time.Sleep(500 * time.Millisecond)
	}
	log.Printf("[EmblemHealth] In-network accordion state found=%t clicked=%t expanded=%q candidates=%d sections=%q url=%s", lastState.Found, lastState.Clicked, lastState.Expanded, lastState.Candidates, lastState.Sections, lastState.URL)
	return false, nil
}

// collectDetailResponses polls until all Vlocity detail methods appear in the captured
// response buffer (or until timeout). Returns whichever methods were captured.
func collectDetailResponses(page *rod.Page, timeout time.Duration) map[string]json.RawMessage {
	result := map[string]json.RawMessage{}
	deadline := time.Now().Add(timeout)
	nextLog := time.Now()
	for time.Now().Before(deadline) {
		res, err := page.Eval(`() => {
			const contexts = [window];
			for (const f of document.querySelectorAll('iframe')) {
				try { if (f.contentWindow) contexts.push(f.contentWindow); } catch(e) {}
			}
			const all = {};
			for (const win of contexts) {
				for (const entry of (win.__emblemApexResponses || [])) {
					if (entry && entry.method && entry.text) {
						all[entry.method] = entry.text;
					}
				}
			}
			return JSON.stringify(all);
		}`)
		if err == nil {
			var all map[string]string
			if json.Unmarshal([]byte(res.Value.Str()), &all) == nil {
				for _, method := range detailMethods {
					if _, ok := result[method]; !ok {
						if text, ok2 := all[method]; ok2 && strings.TrimSpace(text) != "" {
							result[method] = json.RawMessage(text)
						}
					}
				}
			}
		}
		if len(result) == len(detailMethods) {
			log.Printf("[EmblemHealth] all %d detail responses collected", len(detailMethods))
			break
		}
		if time.Now().After(nextLog) {
			log.Printf("[EmblemHealth] waiting for detail responses: %d/%d", len(result), len(detailMethods))
			nextLog = time.Now().Add(5 * time.Second)
		}
		time.Sleep(500 * time.Millisecond)
	}
	if len(result) > 0 && len(result) < len(detailMethods) {
		log.Printf("[EmblemHealth] detail responses partial: %d/%d methods captured", len(result), len(detailMethods))
	}
	return result
}

// clickDetailPageTabs clicks every collapsed accordion section (down-arrow expand button)
// in the member detail view, pausing after each click so that lazy-loaded dental-benefit
// API calls (InNetwork, OutNetwork, Limitations, ToothHistory, etc.) have time to fire.
func clickDetailPageTabs(page *rod.Page) {
	// Selectors for Salesforce Lightning / Vlocity accordion expand triggers.
	// We collect all of them, click each one, then wait.
	res, err := page.Eval(`() => {
		const contexts = [{doc: document, win: window}];
		for (const f of document.querySelectorAll('iframe')) {
			try { if (f.contentDocument) contexts.push({doc: f.contentDocument, win: f.contentWindow}); } catch(e) {}
		}
		const clicked = [];
		for (const {doc, win} of contexts) {
			// Accordion / collapsible expand triggers:
			//   - SLDS accordion summary buttons
			//   - aria-expanded="false" buttons/anchors (collapsed sections)
			//   - Generic chevron/arrow icons inside buttons
			const candidates = Array.from(doc.querySelectorAll(
				'button.slds-accordion__summary-action, ' +
				'button[aria-expanded="false"], ' +
				'a[aria-expanded="false"], ' +
				'[role="button"][aria-expanded="false"], ' +
				'.slds-accordion__section:not(.slds-is-open) button, ' +
				'.accordion-header button, ' +
				'.collapse-toggle, ' +
				'button:has(svg.slds-button__icon_right), ' +
				'button:has(.slds-icon-utility-chevrondown), ' +
				'button:has(.fa-chevron-down), ' +
				'button:has(.fa-angle-down)'
			));
			// Also catch any button whose text/aria-label contains a down-arrow character
			const allBtns = Array.from(doc.querySelectorAll('button, [role="button"], a'));
			for (const btn of allBtns) {
				const text = btn.innerText || btn.textContent || btn.getAttribute('aria-label') || '';
				if (/▼|⌄|chevron.?down|expand/i.test(text) && !candidates.includes(btn)) {
					candidates.push(btn);
				}
			}
			for (const el of candidates) {
				if (!el || el.disabled || el.getAttribute('aria-disabled') === 'true') continue;
				if (el.getAttribute('aria-expanded') === 'true') continue; // already open
				const label = (el.innerText || el.textContent || el.getAttribute('aria-label') || '').trim().slice(0, 60);
				el.scrollIntoView({block: 'center'});
				el.dispatchEvent(new MouseEvent('mousedown', {bubbles: true, cancelable: true, view: win}));
				el.dispatchEvent(new MouseEvent('mouseup',  {bubbles: true, cancelable: true, view: win}));
				el.dispatchEvent(new MouseEvent('click',    {bubbles: true, cancelable: true, view: win}));
				if (typeof el.click === 'function') el.click();
				clicked.push(label || '(no label)');
			}
		}
		return JSON.stringify(clicked);
	}`)
	if err != nil {
		log.Printf("[EmblemHealth] clickDetailPageTabs eval error: %v", err)
		return
	}
	var sections []string
	if json.Unmarshal([]byte(res.Value.Str()), &sections) == nil && len(sections) > 0 {
		log.Printf("[EmblemHealth] expanded %d accordion sections: %v", len(sections), sections)
		// Pause after expanding so each section's lazy API calls have time to fire.
		time.Sleep(time.Duration(len(sections)+1) * 3 * time.Second)
	} else {
		log.Printf("[EmblemHealth] no collapsed accordion sections found")
	}
}

func installApexRemoteBrowserHook(page *rod.Page) error {
	_, err := page.Eval(`() => {
		const install = (win) => {
			if (!win || win.__emblemApexRemoteHookInstalled) return false;
			win.__emblemApexRemoteHookInstalled = true;
			win.__emblemApexRemoteRequests = win.__emblemApexRemoteRequests || [];
			const remember = (url, body) => {
				try {
					const text = typeof body === 'string' ? body : (body ? String(body) : '');
					if (String(url || '').includes('/ehprovider/apexremote') || text.includes('CardCanvasController') || text.includes('IntegrationProcedureService')) {
						win.__emblemApexRemoteRequests.push({ url: String(url || ''), body: text, at: Date.now() });
					}
				} catch (e) {}
			};
			if (win.XMLHttpRequest && win.XMLHttpRequest.prototype) {
				const proto = win.XMLHttpRequest.prototype;
				const origOpen = proto.open;
				const origSend = proto.send;
				proto.open = function(method, url) {
					this.__emblemApexRemoteURL = url;
					return origOpen.apply(this, arguments);
				};
				proto.send = function(body) {
					remember(this.__emblemApexRemoteURL, body);
					return origSend.apply(this, arguments);
				};
			}
			if (win.fetch) {
				const origFetch = win.fetch;
				win.fetch = function(input, init) {
					const url = typeof input === 'string' ? input : (input && input.url);
					const body = init && init.body;
					remember(url, body);
					return origFetch.apply(this, arguments);
				};
			}
			return true;
		};
		let installed = 0;
		if (install(window)) installed++;
		for (const frame of document.querySelectorAll('iframe')) {
			try { if (install(frame.contentWindow)) installed++; } catch (e) {}
		}
		return installed;
	}`)
	if err == nil {
		log.Printf("[EmblemHealth] warmup browser network hook installed")
	}
	return err
}

func capturedApexCtxFromBrowserHook(page *rod.Page) (emblemApexCtx, bool) {
	res, err := page.Eval(`() => {
		const contexts = [window];
		for (const frame of document.querySelectorAll('iframe')) {
			try { if (frame.contentWindow) contexts.push(frame.contentWindow); } catch (e) {}
		}
		for (const win of contexts) {
			const requests = win.__emblemApexRemoteRequests || [];
			for (let i = requests.length - 1; i >= 0; i--) {
				const body = requests[i] && requests[i].body;
				if (body) return body;
			}
		}
		return '';
	}`)
	if err != nil {
		return emblemApexCtx{}, false
	}
	body := strings.TrimSpace(res.Value.Str())
	if body == "" {
		return emblemApexCtx{}, false
	}
	log.Printf("[EmblemHealth] warmup browser hook captured body summary=%s", summarizeApexRemoteBody(body))
	var req struct {
		Ctx emblemApexCtx `json:"ctx"`
	}
	if json.Unmarshal([]byte(body), &req) != nil || req.Ctx.Authorization == "" {
		return emblemApexCtx{}, false
	}
	return req.Ctx, true
}

func summarizeApexRemoteBody(body string) string {
	var req struct {
		Action string        `json:"action"`
		Method string        `json:"method"`
		Data   []string      `json:"data"`
		Ctx    emblemApexCtx `json:"ctx"`
	}
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		return fmt.Sprintf("json=false prefix=%q", safeBodyPrefix(body, 160))
	}
	return fmt.Sprintf("json=true action=%q method=%q data=%q ctxAuth=%t ctxCSRF=%t ctxVID=%q",
		req.Action, req.Method, apexDataMethod(req.Data), req.Ctx.Authorization != "", req.Ctx.CSRF != "", req.Ctx.VID)
}

func rawCtxFromApexBody(body string) json.RawMessage {
	var req map[string]json.RawMessage
	if json.Unmarshal([]byte(body), &req) != nil {
		return nil
	}
	return req["ctx"]
}

func apexDataMethod(data []string) string {
	if len(data) > 1 {
		return data[1]
	}
	return ""
}

func safeBodyPrefix(body string, limit int) string {
	body = strings.ReplaceAll(body, "\r", "")
	body = strings.ReplaceAll(body, "\n", " ")
	if len(body) > limit {
		return body[:limit] + "..."
	}
	return body
}

// fillAndClickEligibilityForm waits for the eligibility form, fills memberID, and clicks Search.
// Primary: JS from main frame traversing same-origin iframes via contentDocument/contentWindow.
// Uses AngularJS scope API (scope.memberInfo.memId / scope.searchElg()) when available.
// Fallback: rod CDP frame API for cross-origin iframes.
func fillAndClickEligibilityForm(page *rod.Page, memberID string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		// Strategy 1: main-frame JS that traverses same-origin iframes.
		// For each context we carry both doc and win so we can access the iframe's angular instance.
		res, err := page.Eval(`(val) => {
			const contexts = [{doc: document, win: window}];
			for (const f of document.querySelectorAll('iframe')) {
				try { if (f.contentDocument) contexts.push({doc: f.contentDocument, win: f.contentWindow}); } catch(e) {}
			}
			const iframeCount = document.querySelectorAll('iframe').length;
			for (const {doc, win} of contexts) {
				const el = doc.querySelector('#bulkeligiblityMemberID');
				if (!el) continue;
				try {
					const scope = win.angular.element(el).scope();
					scope.memberInfo.memId = val;
					scope.$apply();
				} catch(e) {
					el.value = val;
					try { el.dispatchEvent(new win.Event('input', {bubbles: true})); } catch(e2) {
						el.dispatchEvent(new Event('input', {bubbles: true}));
					}
					el.dispatchEvent(new Event('change', {bubbles: true}));
				}
				return JSON.stringify({ok: true, iframeCount});
			}
			return JSON.stringify({ok: false, iframeCount});
		}`, memberID)
		if err == nil {
			var result struct {
				OK          bool `json:"ok"`
				IframeCount int  `json:"iframeCount"`
			}
			if json.Unmarshal([]byte(res.Value.Str()), &result) == nil && result.OK {
				_, _ = page.Eval(`() => {
					const contexts = [{doc: document, win: window}];
					for (const f of document.querySelectorAll('iframe')) {
						try { if (f.contentDocument) contexts.push({doc: f.contentDocument, win: f.contentWindow}); } catch(e) {}
					}
					for (const {doc, win} of contexts) {
						const btn = doc.querySelector('#search');
						if (!btn || btn.disabled) continue;
						try { win.angular.element(btn).scope().searchElg(); return true; } catch(e) {}
						btn.click();
						return true;
					}
					return false;
				}`)
				return nil
			}
			log.Printf("[EmblemHealth] waiting for eligibility form via JS (iframes=%d)", result.IframeCount)
		}

		// Strategy 2: rod CDP frame API — works for cross-origin iframes.
		if frames, ferr := page.Elements("iframe"); ferr == nil {
			for i, f := range frames {
				fp, ferr := f.Frame()
				if ferr != nil {
					log.Printf("[EmblemHealth] iframe[%d] CDP frame error: %v", i, ferr)
					continue
				}
				filled, _ := fp.Eval(`(val) => {
					const el = document.querySelector('#bulkeligiblityMemberID');
					if (!el) return false;
					try {
						const scope = window.angular.element(el).scope();
						scope.memberInfo.memId = val;
						scope.$apply();
					} catch(e) {
						el.value = val;
						el.dispatchEvent(new Event('input', {bubbles: true}));
					}
					return true;
				}`, memberID)
				if filled != nil && filled.Value.Bool() {
					log.Printf("[EmblemHealth] member ID filled via CDP iframe[%d]", i)
					clickSearchBtn(fp)
					return nil
				}
			}
		}

		time.Sleep(2 * time.Second)
	}
	return fmt.Errorf("[EmblemHealth] eligibility form not accessible after %s", timeout)
}

func clickSearchBtn(page *rod.Page) {
	clicked, _ := page.Eval(`() => {
		const btn = document.querySelector('#search');
		if (!btn || btn.disabled) return false;
		try { window.angular.element(btn).scope().searchElg(); return true; } catch(e) {}
		btn.click();
		return true;
	}`)
	if clicked == nil || !clicked.Value.Bool() {
		if btn, err := firstElementWithTimeout(page, []string{`#search`, `button`}, 3*time.Second); err == nil {
			_ = btn.Click(proto.InputMouseButtonLeft, 1)
		}
	}
}

func invokeBulkEligibilityHTTP(apexCtx emblemApexCtx, cookies []*proto.NetworkCookie, appointments []models.Appointment) (*emapi.InvokeResult, string, error) {
	reqJSON, err := buildBulkEligibilityApexRequest(apexCtx, appointments)
	if err != nil {
		return nil, "", err
	}
	req, err := http.NewRequest(http.MethodPost, apexRemoteURL, bytes.NewReader(reqJSON))
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "*/*")
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	req.Header.Set("X-User-Agent", "Visualforce-Remoting")
	req.Header.Set("Origin", providerBaseURL)
	req.Header.Set("Referer", eligibilityPageURL)
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/147.0.0.0 Safari/537.36")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")
	for _, c := range cookies {
		req.AddCookie(&http.Cookie{Name: c.Name, Value: c.Value})
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("apexremote POST: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, "", fmt.Errorf("read apexremote response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, string(raw), fmt.Errorf("apexremote HTTP %d: %.200s", resp.StatusCode, raw)
	}
	parsed, err := emapi.ParseApexResult(string(raw))
	if err != nil {
		log.Printf("[EmblemHealth] apexremote parse failed: %v rawPrefix=%q", err, safeBodyPrefix(string(raw), 500))
	}
	return parsed, string(raw), err
}

func invokeBulkEligibilityBrowser(page *rod.Page, apexCtx emblemApexCtx, appointments []models.Appointment) (*emapi.InvokeResult, string, error) {
	reqJSON, err := buildBulkEligibilityApexRequest(apexCtx, appointments)
	if err != nil {
		return nil, "", err
	}
	res, err := page.Timeout(120*time.Second).Eval(`async (url, body) => {
		const response = await fetch(url, {
			method: 'POST',
			credentials: 'include',
			headers: {
				'accept': '*/*',
				'content-type': 'application/json',
				'x-requested-with': 'XMLHttpRequest',
				'x-user-agent': 'Visualforce-Remoting'
			},
			body
		});
		const text = await response.text();
		return JSON.stringify({ status: response.status, text });
	}`, apexRemoteURL, string(reqJSON))
	if err != nil {
		return nil, "", fmt.Errorf("browser apexremote POST: %w", err)
	}
	var envelope struct {
		Status int    `json:"status"`
		Text   string `json:"text"`
	}
	if err := json.Unmarshal([]byte(res.Value.Str()), &envelope); err != nil {
		return nil, res.Value.Str(), fmt.Errorf("decode browser apexremote envelope: %w", err)
	}
	if envelope.Status != http.StatusOK {
		return nil, envelope.Text, fmt.Errorf("browser apexremote HTTP %d: %.200s", envelope.Status, envelope.Text)
	}
	parsed, err := emapi.ParseApexResult(envelope.Text)
	if err != nil {
		log.Printf("[EmblemHealth] browser apexremote parse failed: %v rawPrefix=%q", err, safeBodyPrefix(envelope.Text, 500))
	}
	return parsed, envelope.Text, err
}

func buildBulkEligibilityApexRequest(apexCtx emblemApexCtx, appointments []models.Appointment) ([]byte, error) {
	payload := map[string]any{
		"size":       true,
		"fieldNames": "",
		"fieldLabel": "",
	}
	for i, appt := range appointments {
		payload[fmt.Sprintf("memberId%d", i+1)] = strings.TrimSpace(appt.SubscriberID)
	}
	payloadJSON, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	reqBody := map[string]any{
		"action": apexAction,
		"method": "doGenericInvoke",
		"ctx":    apexCtx,
		"data": []any{
			"vlocity_ins.IntegrationProcedureService",
			bulkMethod,
			string(payloadJSON),
			"{}",
		},
		"type": "rpc",
		"tid":  1,
	}
	return json.Marshal(reqBody)
}

func (a *Adapter) runPhase2Only(ctx context.Context, input payers.SessionInput) (payers.RunSummary, error) {
	var summary payers.RunSummary
	outputDir := filepath.Join("artifacts", "results")
	if err := os.MkdirAll(outputDir, 0o755); err != nil {
		return summary, fmt.Errorf("[EmblemHealth] create results dir: %w", err)
	}
	appointments := input.Appointments
	if len(appointments) == 0 {
		appointments = payers.ScanProbeAppointments(input.ProbeOutputDir, input.Payer.PayerURL, "api_probe")
	}
	writer, writerErr := resultwriter.New(input.Testing, input.ScraperConfig.APIs)
	if writerErr != nil {
		log.Printf("[EmblemHealth] resultwriter unavailable: %v", writerErr)
	}

	for _, appt := range appointments {
		select {
		case <-ctx.Done():
			return summary, ctx.Err()
		default:
		}
		probePath := payers.ProbeFilePathForAppointment(input.ProbeOutputDir, input.Payer.PayerURL, appt, "api_probe")
		bundle, readErr := readProbeBundle(probePath)
		var report *advanced.PatientEligibilityReport
		statusOverride := ""
		if readErr != nil {
			log.Printf("[EmblemHealth] probe read failed patNum=%s aptNum=%s: %v", appt.PatNum, appt.AptNum, readErr)
			if probeErr, err := payers.ReadProbeErrorForAppointment(input.ProbeOutputDir, input.Payer.PayerURL, appt); err == nil {
				statusOverride = resultwriter.StatusForProbeErrorType(probeErr.ErrorType)
			}
			report = payers.BuildUnableToDetermineReport(appt)
		} else if bundle.Record == nil {
			report = payers.BuildNotFoundReport(appt)
		} else {
			el := emeligibility.BuildEligibilityFromProbe(bundle)
			if el == nil {
				report = payers.BuildUnableToDetermineReport(appt)
			} else if !el.Patient.IsEligible {
				r := payers.BuildNotActiveReport(appt, el.Plan.PlanName, el.Plan.Carrier, el.Plan.GroupName)
				r.Patient.FullName = el.Patient.FullName
				r.Patient.MemberID = el.Patient.MemberID
				r.Patient.DateOfBirth = el.Patient.DateOfBirth
				report = r
			} else {
				report = payers.BuildActiveReport(appt, el.Plan.PlanName, el.Plan.Carrier, el.Plan.GroupName)
			}
			writeResultsFile(outputDir, fmt.Sprintf("%s_%s_advanced.json", appt.PatNum, appt.AptNum), report)
		}
		status := apptStatus(report)
		if statusOverride != "" {
			status = statusOverride
		}
		summary.RecordAppointment(appt, status)
		log.Printf("[EmblemHealth] %s %s -> %s", appt.PatNum, appt.AptNum, status)
		if writer != nil {
			writer.ApplyResult(appt, status, input.RequestedOfficeKey, nil, false)
		}
		input.QueuePDFTask(appt, status, report, outputDir)
	}
	return summary, nil
}

func chunkAppointments(appts []models.Appointment, size int) [][]models.Appointment {
	if size <= 0 {
		size = emapi.BulkMemberLimit
	}
	var chunks [][]models.Appointment
	for start := 0; start < len(appts); start += size {
		end := start + size
		if end > len(appts) {
			end = len(appts)
		}
		chunks = append(chunks, appts[start:end])
	}
	return chunks
}

func writeProbeBundle(dir, payerURL string, appt models.Appointment, bundle *emapi.ProbeBundle) error {
	path := payers.ProbeFilePathForAppointment(dir, payerURL, appt, "api_probe")
	data, err := json.MarshalIndent(bundle, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal probe: %w", err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("write probe: %w", err)
	}
	return nil
}

func readProbeBundle(path string) (*emapi.ProbeBundle, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read probe: %w", err)
	}
	var bundle emapi.ProbeBundle
	if err := json.Unmarshal(data, &bundle); err != nil {
		return nil, fmt.Errorf("unmarshal probe: %w", err)
	}
	return &bundle, nil
}

func writeProbeError(dir, payerURL string, appt models.Appointment, probeErr error) {
	path := payers.ProbeFilePathForAppointment(dir, payerURL, appt, "probe_error")
	payload := map[string]any{
		"recordedAt":  time.Now().UTC().Format(time.RFC3339),
		"appointment": appt,
		"error":       probeErr.Error(),
		"errorType":   payers.ClassifyProbeError(probeErr),
	}
	data, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return
	}
	_ = os.WriteFile(path, data, 0o644)
}

func writeResultsFile(dir, filename string, payload any) {
	data, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		log.Printf("[EmblemHealth] marshal results %s: %v", filename, err)
		return
	}
	if err := os.WriteFile(filepath.Join(dir, filename), data, 0o644); err != nil {
		log.Printf("[EmblemHealth] write results %s: %v", filename, err)
	}
}

func apptStatus(report *advanced.PatientEligibilityReport) string {
	if report == nil {
		return resultwriter.ApptStatusError
	}
	switch report.Patient.StatusLabel {
	case "Not Found":
		return resultwriter.ApptStatusNotFound
	case "Unable to Determine":
		return resultwriter.ApptStatusError
	default:
		return resultwriter.EligibilityStatus(report.Patient.IsEligible)
	}
}
