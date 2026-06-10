package api

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"insurance-benefit-agent-go/internal/models"
	gbrowser "insurance-benefit-agent-go/internal/payers/guardian/browser"

	"github.com/go-rod/rod"
)

const (
	searchURL       = "https://www.guardiananytime.com/gaprovider/api/multiple-patient/search"
	memberSearchURL = "https://www.guardiananytime.com/gaprovider/api/provider/member-search"
	dentalPPOURL    = "https://www.guardiananytime.com/gaprovider/api/dental-vob/ppo"
)

type BrowserClient struct {
	page *rod.Page
}

func NewBrowserClient(session *gbrowser.Session) *BrowserClient {
	return &BrowserClient{page: session.Page()}
}

func (c *BrowserClient) Probe(appt models.Appointment) (*ProbeBundle, error) {
	memberID := strings.TrimSpace(appt.SubscriberID)
	if memberID == "" {
		return nil, fmt.Errorf("subscriber ID is required")
	}
	search, err := c.Search(memberID)
	if err != nil {
		return nil, err
	}
	selected := SelectSearchMember(search, appt.FName, appt.LName, appt.DOB)
	if selected == nil {
		return &ProbeBundle{
			Search:     search,
			RecordedAt: time.Now().UTC().Format(time.RFC3339),
		}, nil
	}
	member, err := c.MemberSearch(MemberSearchRequest{
		DivisionID: selected.DivisionID,
		GroupID:    selected.GroupPolicyNumber,
		MemberID:   selected.Identifier,
	})
	if err != nil {
		return nil, err
	}
	dental, err := c.DentalPPO(DentalPPORequest{
		GroupPolicyNumber:       selected.GroupPolicyNumber,
		PatientRelationToMember: selected.Relationship,
		PatientDateOfBirth:      selected.DateOfBirth,
		PatientFirstName:        selected.FirstName,
		PatientIdentifier:       selected.Identifier,
		PatientLastName:         selected.LastName,
	})
	if err != nil {
		return nil, err
	}
	return &ProbeBundle{
		Search:         search,
		Member:         member,
		DentalPPO:      dental,
		SelectedMember: selected,
		RecordedAt:     time.Now().UTC().Format(time.RFC3339),
	}, nil
}

func (c *BrowserClient) Search(identifier string) (*SearchResponse, error) {
	var out SearchResponse
	if err := c.postJSON(searchURL, []SearchRequest{{Identifier: identifier}}, &out); err != nil {
		return nil, fmt.Errorf("guardian multiple patient search: %w", err)
	}
	return &out, nil
}

func (c *BrowserClient) MemberSearch(req MemberSearchRequest) (*MemberResponse, error) {
	var out MemberResponse
	if err := c.postJSON(memberSearchURL, req, &out); err != nil {
		return nil, fmt.Errorf("guardian member search: %w", err)
	}
	return &out, nil
}

func (c *BrowserClient) DentalPPO(req DentalPPORequest) (*DentalPPOResponse, error) {
	var out DentalPPOResponse
	if err := c.postJSON(dentalPPOURL, req, &out); err != nil {
		return nil, fmt.Errorf("guardian dental PPO VOB: %w", err)
	}
	return &out, nil
}

func (c *BrowserClient) postJSON(url string, payload any, out any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	res, err := c.page.Timeout(90*time.Second).Eval(`async (url, body) => {
		const response = await fetch(url, {
			method: 'POST',
			credentials: 'include',
			headers: {
				'accept': 'application/json',
				'content-type': 'application/json'
			},
			body
		});
		const text = await response.text();
		return JSON.stringify({ status: response.status, text });
	}`, url, string(body))
	if err != nil {
		return err
	}
	var envelope struct {
		Status int    `json:"status"`
		Text   string `json:"text"`
	}
	if err := json.Unmarshal([]byte(res.Value.Str()), &envelope); err != nil {
		return err
	}
	if envelope.Status < 200 || envelope.Status >= 300 {
		return fmt.Errorf("HTTP %d: %.300s", envelope.Status, envelope.Text)
	}
	if err := json.Unmarshal([]byte(envelope.Text), out); err != nil {
		return fmt.Errorf("decode response: %w: %.300s", err, envelope.Text)
	}
	return nil
}

func SelectSearchMember(search *SearchResponse, firstName, lastName, dob string) *SearchMember {
	if search == nil {
		return nil
	}
	firstName = normalizeName(firstName)
	lastName = normalizeName(lastName)
	dob = normalizeDate(dob)
	var fallback *SearchMember
	for i := range search.Results {
		for j := range search.Results[i].MemberDependent {
			m := &search.Results[i].MemberDependent[j]
			if fallback == nil {
				fallback = m
			}
			if normalizeName(m.LastName) == lastName && normalizeDate(m.DateOfBirth) == dob {
				if firstName == "" || strings.Contains(normalizeName(m.FirstName), firstName) || strings.Contains(firstName, normalizeName(m.FirstName)) {
					return m
				}
			}
		}
	}
	return fallback
}

func normalizeName(value string) string {
	return strings.Join(strings.Fields(strings.ToUpper(strings.TrimSpace(value))), " ")
}

func normalizeDate(value string) string {
	value = strings.TrimSpace(value)
	for _, layout := range []string{"01/02/2006", "01-02-2006", "2006-01-02"} {
		if t, err := time.Parse(layout, value); err == nil {
			return t.Format("01/02/2006")
		}
	}
	return value
}
