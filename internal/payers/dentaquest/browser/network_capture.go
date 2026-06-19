package browser

import (
	"encoding/base64"
	"encoding/json"
	"log"
	"sort"
	"strings"
	"time"

	"insurance-benefit-agent-go/internal/logging"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/proto"
)

// memberDetailEndpoints are the XHR endpoint name fragments captured during
// member-details page navigation.
var memberDetailEndpoints = []string{
	"member-info",
	"plan-info",
	"family-info",
	"plan-benefit-summary",
	"member-eligibility",
	"maximum-deductible",
	"clinical-history",
	"claim-auth-history",
	"treatment-plan-estimate-history",
	"enrollment-history",
	"coordination-of-benefits",
}

// CapturedPayload holds one parsed network response for a member-details endpoint.
type CapturedPayload struct {
	Payload    any
	URL        string
	CapturedAt time.Time
}

// startNetworkCapture wires up response interception for DentaQuest member-details
// XHR/fetch responses across all browser targets.
func (s *Session) startNetworkCapture() {
	if s == nil || s.browser == nil || s.browser.Browser == nil {
		return
	}

	// Register one browser-wide listener. Actual response handling happens in
	// captureNetworkResponse whenever Chrome emits NetworkResponseReceived.
	go s.browser.Browser.EachEvent(func(e *proto.NetworkResponseReceived, sessionID proto.TargetSessionID) {
		s.captureNetworkResponse(e, sessionID)
	})()
}

// ClearPayloads resets the payload store. Call this before navigating to a
// new member-details page so stale data from the previous member is discarded.
func (s *Session) ClearPayloads() {
	s.payloadsMu.Lock()
	s.payloads = make(map[string]*CapturedPayload)
	s.lastPayloadAt = time.Time{}
	s.captureMemberDetails = false
	s.payloadsMu.Unlock()
}

// GetPayload returns the captured payload for the given endpoint name, or nil
// if no response has been captured for that endpoint.
func (s *Session) GetPayload(name string) *CapturedPayload {
	s.payloadsMu.Lock()
	defer s.payloadsMu.Unlock()
	return s.payloads[name]
}

func (s *Session) enableNetworkCaptureOnPage(page *rod.Page) {
	if s == nil || page == nil {
		return
	}
	// Without enabling the Network domain on the active page, the browser-wide
	// listener will not receive that page's response events.
	page.EnableDomain(&proto.NetworkEnable{})
}

func (s *Session) captureNetworkResponse(e *proto.NetworkResponseReceived, sessionID proto.TargetSessionID) {
	if s == nil || s.browser == nil || s.browser.Browser == nil || e == nil {
		return
	}
	if e.Type != proto.NetworkResourceTypeXHR && e.Type != proto.NetworkResourceTypeFetch {
		return
	}

	endpoint := memberDetailEndpointForURL(e.Response.URL)
	if endpoint == "" {
		return
	}

	// Resolve the page that produced this response event.
	page := s.browser.Browser.PageFromSession(sessionID)
	if page == nil {
		return
	}
	if !s.shouldCaptureMemberDetails() {
		return
	}

	// Read the raw response body back from Chrome for this request.
	body, err := readResponseBodyWithRetry(page, e.RequestID)
	if err != nil {
		log.Printf("[DentaQuest] read %s payload body failed: %v", endpoint, err)
		return
	}

	raw := body.Body
	if body.Base64Encoded {
		decoded, decodeErr := base64.StdEncoding.DecodeString(raw)
		if decodeErr != nil {
			log.Printf("[DentaQuest] decode %s payload body failed: %v", endpoint, decodeErr)
			return
		}
		raw = string(decoded)
	}

	var payload any
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		log.Printf("[DentaQuest] parse %s payload JSON failed: %v", endpoint, err)
		return
	}

	// Store the parsed payload by endpoint name for the scraper to consume later.
	s.payloadsMu.Lock()
	s.payloads[endpoint] = &CapturedPayload{
		Payload:    payload,
		URL:        e.Response.URL,
		CapturedAt: time.Now(),
	}
	s.lastPayloadAt = time.Now()
	s.payloadsMu.Unlock()

	if endpoint == "plan-benefit-summary" {
		logging.Info("dentaquest.browser", "dentaquest.member.payload.plan_benefit_summary_captured", "captured plan benefit summary payload", map[string]any{
			"url": e.Response.URL,
		})
	}
}

func readResponseBodyWithRetry(page *rod.Page, requestID proto.NetworkRequestID) (*proto.NetworkGetResponseBodyResult, error) {
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		body, err := proto.NetworkGetResponseBody{RequestID: requestID}.Call(page)
		if err == nil {
			return body, nil
		}
		lastErr = err
		// Chrome can briefly report that the response body is not available yet.
		time.Sleep(time.Duration(150*(attempt+1)) * time.Millisecond)
	}
	return nil, lastErr
}

func memberDetailEndpointForURL(rawURL string) string {
	lowerURL := strings.ToLower(rawURL)
	if strings.Contains(lowerURL, "/eligibility/member-search") {
		return ""
	}
	if strings.Contains(lowerURL, "/eligibility/member-eligibility") {
		return "member-eligibility"
	}
	if !strings.Contains(lowerURL, "/member-detail/") {
		return ""
	}
	for _, endpoint := range memberDetailEndpoints {
		if endpoint == "member-eligibility" {
			continue
		}
		if strings.Contains(lowerURL, "/"+endpoint) {
			return endpoint
		}
	}
	return ""
}

func (s *Session) hasPayload(name string) bool {
	if s == nil {
		return false
	}
	s.payloadsMu.Lock()
	defer s.payloadsMu.Unlock()
	_, ok := s.payloads[name]
	return ok
}

func (s *Session) payloadCount() int {
	if s == nil {
		return 0
	}
	s.payloadsMu.Lock()
	defer s.payloadsMu.Unlock()
	return len(s.payloads)
}

func (s *Session) payloadState() (count int, lastAt time.Time) {
	if s == nil {
		return 0, time.Time{}
	}
	s.payloadsMu.Lock()
	defer s.payloadsMu.Unlock()
	return len(s.payloads), s.lastPayloadAt
}

func (s *Session) payloadNames() []string {
	if s == nil {
		return nil
	}
	s.payloadsMu.Lock()
	defer s.payloadsMu.Unlock()
	names := make([]string, 0, len(s.payloads))
	for name := range s.payloads {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func (s *Session) shouldCaptureMemberDetails() bool {
	if s == nil {
		return false
	}
	s.payloadsMu.Lock()
	defer s.payloadsMu.Unlock()
	return s.captureMemberDetails
}

func (s *Session) setCaptureMemberDetails(enabled bool) {
	if s == nil {
		return
	}
	s.payloadsMu.Lock()
	s.captureMemberDetails = enabled
	s.payloadsMu.Unlock()
}

func (s *Session) waitForMemberDetailPayloads(timeout time.Duration) {
	if s == nil {
		return
	}

	startedAt := time.Now()
	deadline := startedAt.Add(timeout)
	benefitSummaryDeadline := startedAt.Add(8 * time.Second)
	if benefitSummaryDeadline.After(deadline) {
		benefitSummaryDeadline = deadline
	}
	requiredSeenAt := time.Time{}
	for time.Now().Before(deadline) {
		count, lastAt := s.payloadState()
		haveSummary := s.hasPayload("member-info") || s.hasPayload("member-eligibility")
		havePlan := s.hasPayload("plan-info") || s.hasPayload("plan-benefit-summary")
		haveHistory := s.hasPayload("clinical-history") || s.hasPayload("claim-auth-history") || s.hasPayload("maximum-deductible")
		haveBenefitSummary := s.hasPayload("plan-benefit-summary")

		if !haveBenefitSummary && time.Now().Before(benefitSummaryDeadline) {
			time.Sleep(200 * time.Millisecond)
			continue
		}

		if haveSummary && havePlan && haveHistory {
			if requiredSeenAt.IsZero() {
				requiredSeenAt = time.Now()
			}
			quiet := !lastAt.IsZero() && time.Since(lastAt) >= 500*time.Millisecond
			lingerSatisfied := time.Since(requiredSeenAt) >= 1500*time.Millisecond
			if !haveBenefitSummary {
				// plan-benefit-summary is often the last useful payload and it
				// can arrive noticeably after the core member-detail payloads.
				lingerSatisfied = time.Since(requiredSeenAt) >= 4500*time.Millisecond
			}
			if quiet || lingerSatisfied {
				log.Printf("[DentaQuest] member-detail payloads ready: count=%d plan-benefit-summary=%t", count, haveBenefitSummary)
				logging.Info("dentaquest.browser", "dentaquest.member.payload.ready", "member detail payloads ready", map[string]any{
					"count":              count,
					"planBenefitSummary": haveBenefitSummary,
				})
				return
			}
		}

		time.Sleep(200 * time.Millisecond)
	}

	count, _ := s.payloadState()
	log.Printf("[DentaQuest] member-detail payload wait timed out: count=%d plan-benefit-summary=%t", count, s.hasPayload("plan-benefit-summary"))
	logging.Warn("dentaquest.browser", "dentaquest.member.payload.timeout", "member detail payload wait timed out", map[string]any{
		"count":              count,
		"planBenefitSummary": s.hasPayload("plan-benefit-summary"),
	})
}
