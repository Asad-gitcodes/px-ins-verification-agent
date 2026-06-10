package api

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"insurance-benefit-agent-go/internal/models"
	dbrowser "insurance-benefit-agent-go/internal/payers/dentical/browser"

	"github.com/go-rod/rod"
)

const graphqlURL = "/graphql"

type BrowserClient struct {
	page *rod.Page
}

func NewBrowserClient(session *dbrowser.Session) *BrowserClient {
	return &BrowserClient{page: session.Page()}
}

func (c *BrowserClient) Probe(appt models.Appointment, providerID string) (*ProbeBundle, error) {
	subscriberID := strings.TrimSpace(appt.SubscriberID)
	if subscriberID == "" {
		return nil, fmt.Errorf("subscriber ID is required")
	}
	birthDate := normalizeDate(firstNonEmpty(appt.DOB, appt.SubDOB))
	if birthDate == "" {
		return nil, fmt.Errorf("date of birth is required")
	}
	providerID = strings.TrimSpace(providerID)
	if providerID == "" {
		return nil, fmt.Errorf("provider ID/NPI is required")
	}
	profile, err := c.Profile()
	if err != nil {
		return nil, err
	}
	if profile.OrgID == "" || profile.UserID == "" {
		return nil, fmt.Errorf("Denti-Cal profile missing org/user IDs")
	}
	today := time.Now().Format("01/02/2006")
	req := EligibilityRequest{
		SubscriberID: subscriberID,
		IssueDate:    today,
		BirthDate:    birthDate,
		ServiceDate:  today,
		ProviderID:   providerID,
		OrgID:        profile.OrgID,
		OrgUserID:    profile.UserID,
	}
	response, err := c.Eligibility(req)
	if err != nil {
		return nil, err
	}
	return &ProbeBundle{
		Appointment: appt,
		Profile:     profile,
		Request:     req,
		Response:    response,
		RecordedAt:  time.Now().UTC().Format(time.RFC3339),
	}, nil
}

func (c *BrowserClient) Profile() (*Profile, error) {
	var out struct {
		AccountManagement struct {
			GetUserProfile struct {
				UserID         string `json:"userId"`
				LoggedIn       bool   `json:"loggedIn"`
				Role           string `json:"role"`
				Persona        string `json:"persona"`
				PersonaOrgType string `json:"personaOrgType"`
				GivenName      string `json:"givenName"`
				FamilyName     string `json:"familyName"`
			} `json:"getUserProfile"`
			GetInternetAgreement struct {
				InetAgreement struct {
					OrgID string `json:"orgId"`
				} `json:"inetAgreement"`
			} `json:"getInternetAgreement"`
		} `json:"accountManagement"`
	}
	if err := c.graphql("", profileQuery, map[string]any{}, &out); err != nil {
		return nil, fmt.Errorf("Denti-Cal profile: %w", err)
	}
	user := out.AccountManagement.GetUserProfile
	inet := out.AccountManagement.GetInternetAgreement.InetAgreement
	return &Profile{
		UserID:    user.UserID,
		OrgID:     inet.OrgID,
		LoggedIn:  user.LoggedIn,
		Role:      user.Role,
		Persona:   user.Persona,
		OrgType:   user.PersonaOrgType,
		FirstName: user.GivenName,
		LastName:  user.FamilyName,
	}, nil
}

func (c *BrowserClient) Eligibility(req EligibilityRequest) (*EligibilityStatus, error) {
	var out struct {
		SubscriberEligibility struct {
			GetEligibilityStatus EligibilityStatus `json:"getEligibilityStatus"`
		} `json:"subscriberEligibility"`
	}
	variables := map[string]any{
		"subscriberID": req.SubscriberID,
		"issueDate":    req.IssueDate,
		"birthDate":    req.BirthDate,
		"serviceDate":  req.ServiceDate,
		"providerID":   req.ProviderID,
		"orgID":        req.OrgID,
		"orgUserID":    req.OrgUserID,
	}
	if err := c.graphql("subscriberEligibility", eligibilityQuery, variables, &out); err != nil {
		return nil, fmt.Errorf("Denti-Cal eligibility: %w", err)
	}
	return &out.SubscriberEligibility.GetEligibilityStatus, nil
}

func (c *BrowserClient) graphql(operationName, query string, variables map[string]any, out any) error {
	payload := map[string]any{
		"query":     query,
		"variables": variables,
	}
	if operationName != "" {
		payload["operationName"] = operationName
	}
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
		return JSON.stringify({status: response.status, text});
	}`, graphqlURL, string(body))
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
	var graph struct {
		Data   json.RawMessage `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal([]byte(envelope.Text), &graph); err != nil {
		return fmt.Errorf("decode GraphQL envelope: %w: %.300s", err, envelope.Text)
	}
	if len(graph.Errors) > 0 {
		return fmt.Errorf("GraphQL error: %s", graph.Errors[0].Message)
	}
	if len(graph.Data) == 0 || string(graph.Data) == "null" {
		return fmt.Errorf("GraphQL response missing data")
	}
	if err := json.Unmarshal(graph.Data, out); err != nil {
		return fmt.Errorf("decode GraphQL data: %w: %.300s", err, string(graph.Data))
	}
	return nil
}

func normalizeDate(value string) string {
	value = strings.TrimSpace(value)
	for _, layout := range []string{"01/02/2006", "1/2/2006", "2006-01-02", "01-02-2006", "1-2-2006"} {
		t, err := time.Parse(layout, value)
		if err == nil {
			return t.Format("01/02/2006")
		}
	}
	return value
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" {
			return value
		}
	}
	return ""
}

const profileQuery = `{
  accountManagement {
    getUserProfile {
      userId
      loggedIn
      role
      persona
      personaOrgType
      givenName
      familyName
      __typename
    }
    getInternetAgreement {
      inetAgreement {
        orgId
        __typename
      }
      __typename
    }
    __typename
  }
}`

const eligibilityQuery = `query subscriberEligibility($subscriberID: String, $issueDate: String, $birthDate: String, $serviceDate: String, $providerID: String, $orgID: String, $orgUserID: String) {
  subscriberEligibility {
    getEligibilityStatus(
      subscriberID: $subscriberID
      issueDate: $issueDate
      birthDate: $birthDate
      serviceDate: $serviceDate
      providerID: $providerID
      orgID: $orgID
      orgUserID: $orgUserID
    ) {
      status
      results {
        errorCode
        evcTraceNumber
        serviceDate
        name {
          lastName
          firstName
          initial
          __typename
        }
        birthDate
        foundElig
        subscriberID
        submittedID
        medicareID
        issueDate
        percentObligation
        totalObligation
        remainingSOC
        serviceType
        socInfo {
          caseNum
          balance
          __typename
        }
        eligTransPerfBy
        eligibilityCode
        PCPPhone
        eligibilityCodesForMonth {
          countyCode
          primaryAid
          firstAid
          secondAid
          thirdAid
          __typename
        }
        textMessage
        textMessageCode
        __typename
      }
      errors {
        subscriberID
        issueDate
        birthDate
        serviceDate
        __typename
      }
      __typename
    }
    __typename
  }
}`
