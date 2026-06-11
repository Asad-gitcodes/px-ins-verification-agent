package api

import (
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"

	"insurance-benefit-agent-go/internal/models"
	ddbrowser "insurance-benefit-agent-go/internal/payers/deltadentalins/browser"

	"github.com/go-rod/rod"
)

// New portal base — provider.deltadental.com / portal.deltadental.com
const (
	memberSearchURL = "https://portal.deltadental.com/portal/api/v1/providers/edi/memberSearch?includeInactiveMembers=false"
	benefitsURL     = "https://portal.deltadental.com/portal/api/v1/providers/edi/benefits"
)

// ── Request / Response types ──────────────────────────────────────────────────

// MemberSearchRequest is the body sent to /memberSearch.
type MemberSearchRequest struct {
	MemberID  string `json:"memberId"`
	FirstName string `json:"firstName"`
	LastName  string `json:"lastName"`
	DOB       string `json:"dateOfBirth"` // YYYY-MM-DD
}

// MemberSearchResponse is what the portal returns for a successful member lookup.
type MemberSearchResponse struct {
	SubscriberFirstName   string      `json:"subscriberFirstName"`
	SubscriberLastName    string      `json:"subscriberLastName"`
	GroupName             string      `json:"groupName"`
	SubscriberHash        string      `json:"subscriberHash"`
	MemberCompanyName     string      `json:"memberCompanyName"`
	MemberCompanyContext  any         `json:"memberCompanyContext"`
	SubscriberDateOfBirth string      `json:"subscriberDateOfBirth"`
	ActiveStatus          bool        `json:"activeStatus"`
	ZipCode               string      `json:"zipCode"`
	Dependents            []Dependent `json:"dependents"`
}

type Dependent struct {
	FirstName     string `json:"firstName"`
	LastName      string `json:"lastName"`
	DateOfBirth   string `json:"dateOfBirth"`
	MemberID      string `json:"memberId"`
	Relationship  string `json:"relationship"`
	ActiveStatus  bool   `json:"activeStatus"`
	SubscriberHash string `json:"subscriberHash"`
}

// BenefitsResponse is returned by /benefits (structure TBD from network capture).
// Fill in fields as you inspect DevTools → Network → benefits response.
type BenefitsResponse struct {
	Raw map[string]any `json:"raw,omitempty"` // placeholder until fields are mapped
}

// PatientAPIBundle is the full per-appointment probe written to disk.
type PatientAPIBundle struct {
	PayerURL     string               `json:"payerUrl"`
	Appointment  models.Appointment   `json:"appointment"`
	MemberSearch *MemberSearchResponse `json:"memberSearch,omitempty"`
	Benefits     *BenefitsResponse    `json:"benefits,omitempty"`
	FetchedAt    string               `json:"fetchedAt"`
	NotFound     bool                 `json:"notFound,omitempty"`
}

// ── BrowserProbe ──────────────────────────────────────────────────────────────

type BrowserProbe struct {
	session *ddbrowser.Session
}

func NewBrowserProbe(session *ddbrowser.Session) *BrowserProbe {
	return &BrowserProbe{session: session}
}

// SearchAndFetchPatient does:
//  1. POST /memberSearch with member ID + name + DOB
//  2. If found and active, GET /benefits using subscriberHash
func (p *BrowserProbe) SearchAndFetchPatient(appointment models.Appointment) (*PatientAPIBundle, error) {
	bundle := &PatientAPIBundle{
		PayerURL:    "DeltaDentalIns.com",
		Appointment: appointment,
		FetchedAt:   time.Now().UTC().Format(time.RFC3339),
	}

	// ── Step 1: member search ─────────────────────────────────────────────────
	reqBody := MemberSearchRequest{
		MemberID:  strings.TrimSpace(appointment.SubscriberID),
		FirstName: strings.TrimSpace(appointment.FName),
		LastName:  strings.TrimSpace(appointment.LName),
		DOB:       toYYYYMMDD(appointment.DOB),
	}

	var memberResp MemberSearchResponse
	err := p.postJSON(memberSearchURL, reqBody, &memberResp)
	if err != nil {
		// 404 or "not found" body → mark as not found, don't error
		if strings.Contains(err.Error(), "404") || strings.Contains(strings.ToLower(err.Error()), "not found") {
			log.Printf("[DeltaDental] member not found patNum=%s subscriberId=%s",
				appointment.PatNum, appointment.SubscriberID)
			bundle.NotFound = true
			return bundle, nil
		}
		return nil, fmt.Errorf("member search patNum=%s: %w", appointment.PatNum, err)
	}
	bundle.MemberSearch = &memberResp

	log.Printf("[DeltaDental] member found patNum=%s name=%s %s active=%v company=%s",
		appointment.PatNum, memberResp.SubscriberFirstName, memberResp.SubscriberLastName,
		memberResp.ActiveStatus, memberResp.MemberCompanyName)

	if !memberResp.ActiveStatus {
		log.Printf("[DeltaDental] member inactive patNum=%s", appointment.PatNum)
		return bundle, nil
	}

	// ── Step 2: benefits ──────────────────────────────────────────────────────
	if memberResp.SubscriberHash != "" {
		var benResp BenefitsResponse
		benURL := benefitsURL + "?subscriberHash=" + memberResp.SubscriberHash
		if err := p.getJSON(benURL, nil, &benResp); err != nil {
			log.Printf("[DeltaDental] benefits fetch patNum=%s: %v", appointment.PatNum, err)
		} else {
			bundle.Benefits = &benResp
			log.Printf("[DeltaDental] benefits fetched patNum=%s", appointment.PatNum)
		}
	}

	return bundle, nil
}

// ── HTTP helpers (execute through browser page so session cookies are sent) ───

func (p *BrowserProbe) postJSON(rawURL string, body any, out any) error {
	page := p.session.Page()
	if page == nil {
		return fmt.Errorf("browser page is nil")
	}
	bodyBytes, err := json.Marshal(body)
	if err != nil {
		return err
	}
	headers := map[string]string{
		"accept":       "application/json",
		"content-type": "application/json",
	}
	respBody, status, err := postThroughPage(page, rawURL, string(bodyBytes), headers)
	if err != nil {
		return err
	}
	if status == 404 {
		return fmt.Errorf("404 not found")
	}
	if status < 200 || status >= 300 {
		return fmt.Errorf("POST %s status=%d body=%s", rawURL, status, truncate(respBody, 300))
	}
	if out != nil {
		if err := json.Unmarshal([]byte(respBody), out); err != nil {
			return fmt.Errorf("decode %s: %w (body=%s)", rawURL, err, truncate(respBody, 300))
		}
	}
	return nil
}

func (p *BrowserProbe) getJSON(rawURL string, extraHeaders map[string]string, out any) error {
	page := p.session.Page()
	if page == nil {
		return fmt.Errorf("browser page is nil")
	}
	headers := map[string]string{"accept": "application/json"}
	for k, v := range extraHeaders {
		headers[k] = v
	}
	respBody, status, err := getThroughPage(page, rawURL, headers)
	if err != nil {
		return err
	}
	if status < 200 || status >= 300 {
		return fmt.Errorf("GET %s status=%d body=%s", rawURL, status, truncate(respBody, 300))
	}
	if out != nil {
		if err := json.Unmarshal([]byte(respBody), out); err != nil {
			return fmt.Errorf("decode %s: %w (body=%s)", rawURL, err, truncate(respBody, 300))
		}
	}
	return nil
}

func postThroughPage(page *rod.Page, rawURL, body string, headers map[string]string) (string, int, error) {
	quotedURL, _ := json.Marshal(rawURL)
	quotedBody, _ := json.Marshal(body)
	quotedHeaders, _ := json.Marshal(headers)

	js := fmt.Sprintf(`() => fetch(%s, {
		method: "POST",
		credentials: "include",
		headers: %s,
		body: %s
	}).then(async res => JSON.stringify({ status: res.status, text: await res.text() }))`,
		string(quotedURL), string(quotedHeaders), string(quotedBody))

	res, err := page.Eval(js)
	if err != nil {
		return "", 0, fmt.Errorf("POST through page %s: %w", rawURL, err)
	}
	var payload struct {
		Status int    `json:"status"`
		Text   string `json:"text"`
	}
	if err := json.Unmarshal([]byte(res.Value.Str()), &payload); err != nil {
		return "", 0, err
	}
	return payload.Text, payload.Status, nil
}

func getThroughPage(page *rod.Page, rawURL string, headers map[string]string) (string, int, error) {
	quotedURL, _ := json.Marshal(rawURL)
	quotedHeaders, _ := json.Marshal(headers)

	js := fmt.Sprintf(`() => fetch(%s, {
		credentials: "include",
		headers: %s
	}).then(async res => JSON.stringify({ status: res.status, text: await res.text() }))`,
		string(quotedURL), string(quotedHeaders))

	res, err := page.Eval(js)
	if err != nil {
		return "", 0, fmt.Errorf("GET through page %s: %w", rawURL, err)
	}
	var payload struct {
		Status int    `json:"status"`
		Text   string `json:"text"`
	}
	if err := json.Unmarshal([]byte(res.Value.Str()), &payload); err != nil {
		return "", 0, err
	}
	return payload.Text, payload.Status, nil
}

// toYYYYMMDD converts common date formats to YYYY-MM-DD as the new API expects.
func toYYYYMMDD(value string) string {
	v := strings.TrimSpace(value)
	if v == "" {
		return ""
	}
	layouts := []string{
		"2006-01-02",
		"01/02/2006",
		"01-02-2006",
		"1/2/2006",
		"2006/01/02",
	}
	for _, layout := range layouts {
		if t, err := time.Parse(layout, v); err == nil {
			return t.Format("2006-01-02")
		}
	}
	return v
}

func truncate(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	return value[:limit]
}
