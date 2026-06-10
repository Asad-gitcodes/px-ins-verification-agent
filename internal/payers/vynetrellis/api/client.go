package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const (
	baseURL      = "https://trellis.vynetrellis.com"
	loginPath    = "/services/authentication/credentials/SsoLogin"
	practicePath = "/services/Practice"
	patientsPath = "/trellis-eligibility/%d/patients"
	patientPath  = "/trellis-eligibility/%d/patient/%s"
	verifyPath   = "/trellis-eligibility/%d/verify/0"
)

// Client handles all HTTP communication with the Vyne Trellis REST API.
type Client struct {
	http         *http.Client
	token        string
	customerID   int
	practiceInfo *TrellisPracticeInfo
}

func NewClient() *Client {
	return &Client{
		http: &http.Client{Timeout: 30 * time.Second},
	}
}

func (c *Client) CustomerID() int { return c.customerID }

func (c *Client) PracticeInfo() *TrellisPracticeInfo { return c.practiceInfo }

// FetchPracticeInfo retrieves provider NPI, taxonomy code, and provider name for
// the logged-in customer. Call once after Login; non-fatal if the endpoint fails.
func (c *Client) FetchPracticeInfo() error {
	req, err := http.NewRequest(http.MethodGet, baseURL+practicePath, nil)
	if err != nil {
		return err
	}
	q := req.URL.Query()
	q.Set("customerId", fmt.Sprintf("%d", c.customerID))
	req.URL.RawQuery = q.Encode()
	c.setHeaders(req)

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("vyne practice info: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return fmt.Errorf("vyne practice info failed status=%s body=%s", resp.Status, strings.TrimSpace(string(b)))
	}

	var pr PracticeResponse
	if err := json.NewDecoder(resp.Body).Decode(&pr); err != nil {
		return fmt.Errorf("vyne practice info decode: %w", err)
	}
	c.practiceInfo = &pr.Info
	return nil
}

// Login authenticates with Vyne Trellis and stores the access token and customer ID.
func (c *Client) Login(username, password string) error {
	body, err := json.Marshal(LoginRequest{Username: username, Password: password})
	if err != nil {
		return err
	}

	resp, err := c.post(loginPath, body, false)
	if err != nil {
		return fmt.Errorf("vyne login: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return fmt.Errorf("vyne login failed status=%s body=%s", resp.Status, strings.TrimSpace(string(b)))
	}

	var lr LoginResponse
	if err := json.NewDecoder(resp.Body).Decode(&lr); err != nil {
		return fmt.Errorf("vyne login decode: %w", err)
	}
	if lr.AccessToken == "" || lr.CustomerID == 0 {
		return fmt.Errorf("vyne login: empty token or customerID in response")
	}

	c.token = lr.AccessToken
	c.customerID = lr.CustomerID
	return nil
}

// SearchPatients searches the Vyne patient list by name fragment.
// Name format: "LastName, FirstName" or just "LastName".
func (c *Client) SearchPatients(name string) ([]PatientSummary, error) {
	body, err := json.Marshal(PatientSearchRequest{
		CurrentPage: 1,
		PageSize:    50,
		Filters:     PatientFilters{Name: name},
		SortColumn:  SortColumn{Column: "Name", Sort: "asc"},
	})
	if err != nil {
		return nil, err
	}

	path := fmt.Sprintf(patientsPath, c.customerID)
	resp, err := c.post(path, body, true)
	if err != nil {
		return nil, fmt.Errorf("vyne patient search: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return nil, fmt.Errorf("vyne patient search failed status=%s body=%s", resp.Status, strings.TrimSpace(string(b)))
	}

	var result PatientSearchResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("vyne patient search decode: %w", err)
	}
	return result.Data, nil
}

// VynePatientMatch holds the Vyne-side identifiers for a matched patient.
type VynePatientMatch struct {
	PatientId string
	SyncId    string
}

// FindPatient searches for a patient by last name and returns their Vyne identifiers
// if a unique match is found by first name and date of birth. Returns nil if not found.
func (c *Client) FindPatient(lastName, firstName, dob string) *VynePatientMatch {
	results, err := c.SearchPatients(lastName + ", " + firstName)
	if err != nil || len(results) == 0 {
		results, err = c.SearchPatients(lastName)
		if err != nil {
			return nil
		}
	}
	normalizedDOB := normalizeDOBForSearch(dob)
	for _, p := range results {
		if !strings.EqualFold(strings.TrimSpace(p.PatientLastName), strings.TrimSpace(lastName)) {
			continue
		}
		if !strings.EqualFold(strings.TrimSpace(p.PatientFirstName), strings.TrimSpace(firstName)) {
			continue
		}
		if normalizedDOB != "" && normalizeDOBForSearch(p.PatientBirthdate) != normalizedDOB {
			continue
		}
		return &VynePatientMatch{PatientId: p.PatientId, SyncId: p.SyncId}
	}
	return nil
}

// normalizeDOBForSearch strips time portion and normalizes to MM/DD/YYYY for comparison.
func normalizeDOBForSearch(dob string) string {
	dob = strings.TrimSpace(dob)
	if idx := strings.Index(dob, " "); idx > 0 {
		dob = dob[:idx]
	}
	layouts := []struct{ in, out string }{
		{"01/02/2006", "01/02/2006"},
		{"01-02-2006", "01/02/2006"},
		{"2006-01-02", "01/02/2006"},
	}
	for _, l := range layouts {
		if t, err := time.Parse(l.in, dob); err == nil {
			return t.Format(l.out)
		}
	}
	return dob
}

// GetPatient fetches full patient details including VerificationHistory.
func (c *Client) GetPatient(patientID string) (*PatientDetail, error) {
	path := fmt.Sprintf(patientPath, c.customerID, patientID)
	resp, err := c.get(path)
	if err != nil {
		return nil, fmt.Errorf("vyne get patient: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return nil, fmt.Errorf("vyne get patient failed status=%s body=%s", resp.Status, strings.TrimSpace(string(b)))
	}

	var detail PatientDetail
	if err := json.NewDecoder(resp.Body).Decode(&detail); err != nil {
		return nil, fmt.Errorf("vyne get patient decode: %w", err)
	}
	return &detail, nil
}

// CheckEligibility triggers a real-time eligibility check using patient demographics.
// Endpoint: POST /trellis-eligibility/{customerId}/verify/0
// The response HtmlResult contains the full eligibility benefit HTML.
func (c *Client) CheckEligibility(detail *PatientDetail) (*VerifyResponse, error) {
	req := VerifyRequest{
		PatientId:            "0",
		CustomerId:           c.customerID,
		PatientFirstName:     detail.PatientFirstName,
		PatientMiddleName:    detail.PatientMiddleName,
		PatientLastName:      detail.PatientLastName,
		PatientSuffix:        detail.PatientSuffix,
		PatientBirthdate:     detail.PatientBirthdate,
		PatientGender:        detail.PatientGender,
		PatientIsSub:         detail.PatientIsSub,
		SubscriberId:         detail.SubscriberId,
		SubscriberFirstName:  detail.SubscriberFirstName,
		SubscriberMiddleName: detail.SubscriberMiddleName,
		SubscriberLastName:   detail.SubscriberLastName,
		SubscriberSuffix:     detail.SubscriberSuffix,
		SubscriberBirthdate:  detail.SubscriberBirthdate,
		SubscriberGender:     detail.SubscriberGender,
		CarrierId:            detail.CarrierId,
		CarrierName:          detail.CarrierName,
		GroupNumber:          detail.GroupNumber,
		IndividualNpi:        detail.IndividualNpi,
		ProviderFirstName:    detail.ProviderFirstName,
		ProviderLastName:     detail.ProviderLastName,
		SyncId:               detail.SyncId,
		TaxonomyCode:         detail.TaxonomyCode,
	}

	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}

	path := fmt.Sprintf(verifyPath, c.customerID)
	var (
		resp    *http.Response
		lastErr error
	)
	for attempt := 0; attempt < 2; attempt++ {
		if attempt > 0 {
			time.Sleep(10 * time.Second)
		}
		resp, lastErr = c.post(path, body, true)
		if lastErr != nil {
			continue
		}
		if resp.StatusCode == http.StatusGatewayTimeout || resp.StatusCode == http.StatusServiceUnavailable {
			b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
			resp.Body.Close()
			lastErr = fmt.Errorf("vyne verify eligibility failed status=%s body=%s", resp.Status, strings.TrimSpace(string(b)))
			continue
		}
		break
	}
	if lastErr != nil {
		return nil, fmt.Errorf("vyne verify eligibility: %w", lastErr)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return nil, fmt.Errorf("vyne verify eligibility failed status=%s body=%s", resp.Status, strings.TrimSpace(string(b)))
	}

	var record VerifyResponse
	if err := json.NewDecoder(resp.Body).Decode(&record); err != nil {
		return nil, fmt.Errorf("vyne verify eligibility decode: %w", err)
	}
	return &record, nil
}

// ── http helpers ──────────────────────────────────────────────────────────────

func (c *Client) get(path string) (*http.Response, error) {
	req, err := http.NewRequest(http.MethodGet, baseURL+path, nil)
	if err != nil {
		return nil, err
	}
	c.setHeaders(req)
	return c.http.Do(req)
}

func (c *Client) post(path string, body []byte, auth bool) (*http.Response, error) {
	req, err := http.NewRequest(http.MethodPost, baseURL+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if auth {
		c.setHeaders(req)
	}
	return c.http.Do(req)
}

func (c *Client) setHeaders(req *http.Request) {
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
}
