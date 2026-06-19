// Package api implements the UHC Dental HTTP API probe.
//
// Flow per patient:
//  1. POST /apps/dental/member  → member data arrives via Set-Cookie headers
//  2. GET  /apps/dental/benefitsummary      → plan info, coverage %, accumulators
//  3. GET  /apps/dental/utilizationHistory  → all CDT codes + frequency + history
//
// Session cookies (JSESSIONID, affinity, P* provider cookies) come from the
// browser login and are injected into the shared cookie jar before any call.
package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"time"
)

const baseURL = "https://secure.uhcdental.com"

// SessionCookies holds the provider-level cookies extracted from the browser
// after a successful Optum SSO login. These are reused for every patient call.
type SessionCookies struct {
	JSESSIONID string
	Affinity   string
	ProviderID string
	PTIN       string
	OptumID    string
	OptumUUID  string
	Raw        []*http.Cookie // full set for jar injection
}

// MemberInfo holds per-patient data parsed from the member search JSON response.
type MemberInfo struct {
	MemberContrivedKey string // demographics.consumerId
	SubscriberID       string // demographics.subscriberId
	PlanID             string // populated later from eligsummary
	MemberFirstName    string
	MemberLastName     string
	BirthDate          string
	GroupID            string
	GroupName          string
	CoverageStopDate   string // "9999-12-31" = active
}

// IsEligible returns true when coverage stop date is in the future.
func (m *MemberInfo) IsEligible() bool {
	if m.CoverageStopDate == "" {
		return false
	}
	t, err := time.Parse("2006-01-02", m.CoverageStopDate)
	if err != nil {
		return false
	}
	return t.After(time.Now())
}

// FullName returns the member's full name.
func (m *MemberInfo) FullName() string {
	return strings.TrimSpace(m.MemberFirstName + " " + m.MemberLastName)
}

// memberSearchResponse is the JSON body returned by POST /apps/dental/member.
type memberSearchResponse struct {
	Result struct {
		Consumers []struct {
			ConsumerKeys interface{} `json:"consumerKeys"`
			GroupInfo    struct {
				GroupName string `json:"groupName"`
				GroupID   string `json:"groupID"`
			} `json:"groupInfo"`
			Coverages []struct {
				CoverageDates struct {
					StartDate string `json:"startDate"`
					StopDate  string `json:"stopDate"`
				} `json:"coverageDates"`
			} `json:"coverages"`
			Demographics struct {
				ConsumerID   string `json:"consumerId"`
				SubscriberID string `json:"subscriberId"`
				MemberName   struct {
					FirstName string `json:"firstName"`
					LastName  string `json:"lastName"`
				} `json:"memberName"`
				BirthDate string `json:"birthDate"`
			} `json:"demographics"`
		} `json:"consumers"`
	} `json:"result"`
}

// ── API response types ────────────────────────────────────────────────────────

// BenefitSummaryResponse is the parsed body of GET /apps/dental/benefitsummary.
type BenefitSummaryResponse struct {
	Result struct {
		DentalBenefitsAndAccums struct {
			Member struct {
				SubscriberID      string `json:"subscriberId"`
				EmployeeID        string `json:"employeeId"`
				GroupID           string `json:"groupId"`
				GroupName         string `json:"groupName"`
				GroupPolicyNumber string `json:"groupPolicyNumber"`
				PatientName       struct {
					FirstName string `json:"firstName"`
					LastName  string `json:"lastName"`
				} `json:"patientName"`
				MemberSuffix                   string `json:"memberSuffix"`
				MemberEligibilityEffectiveDate string `json:"memberEligibilityEffectiveDate"`
				EligibilityIndicator           string `json:"eligibilityIndicator"`
				EligibilityTermDate            string `json:"eligibilityTermDate"`
				ProductID                      struct {
					CodeValue string `json:"codeValue"`
					CodeDesc  string `json:"codeDesc"`
				} `json:"productId"`
				GroupPlanEffectiveDate        string `json:"groupPlanEffectiveDate"`
				PlanYearBeginDate             string `json:"planYearBeginDate"`
				ProductPlanType               string `json:"productPlanType"`
				ProductPlanTypeDescription    string `json:"productPlanTypeDescription"`
				ProductPlanTypeValueCode1     string `json:"productPlanTypeValueCode1"`
				ProductPlanTypeValueCode1Desc string `json:"productPlanTypeValueCode1Description"`
				PayorID                       string `json:"payorId"`
				ClaimsAddress                 string `json:"claimsAddress"`
				ClaimAcceptingMonths          string `json:"claimAcceptingMonths"`
			} `json:"member"`
			PlanLevelBenefits     []PlanLevelBenefit     `json:"planLevelBenefits"`
			CategoryLevelBenefits []CategoryLevelBenefit `json:"categoryLevelBenefits"`
		} `json:"dentalBenefitsAndAccums"`
		Errors interface{} `json:"errors"`
	} `json:"result"`
}

// PlanLevelBenefit is one entry in planLevelBenefits (one per providerType I/O).
type PlanLevelBenefit struct {
	ProviderType       string `json:"providerType"` // "I" or "O"
	PlanLevelLimitInfo struct {
		LimitType struct {
			CodeValue string `json:"codeValue"`
			CodeDesc  string `json:"codeDesc"`
		} `json:"limitType"`
		LimitIndicator                  string `json:"limitIndicator"`
		LimitMemberAccumNumber          string `json:"limitMemberAccumNumber"`
		LimitMemberAccumDesc            string `json:"limitMemberAccumDesc"`
		OOPFamilyAccumNumber            string `json:"oopFamilyAccumNumber"`
		LimitNonEmbeddedInd             string `json:"limitNonEmbeddedInd"`
		LimitPeriod                     string `json:"limitPeriod"`
		LimitMemberMaxAmt               string `json:"limitMemberMaxAmt"`
		OOPFamilyMaxAmt                 string `json:"oopFamilyMaxAmt"`
		LimitCurrentYear                string `json:"limitCurrentYear"`
		CurrYearLimitMemberAmtSatisfied string `json:"currYearLimitMemberAmtSatisfied"`
		CurrYearOOPFamilyAmtSatisfied   string `json:"currYearOopFamilyAmtSatisfied"`
		LimitPreviousYear               string `json:"limitPreviousYear"`
		PrevYearOOPMemberMaximumAmount  string `json:"prevYearOopMemberMaximumAmount"`
		PrevYearLimitMemberAmtSatisfied string `json:"prevYearLimitMemberAmtSatisfied"`
		RelatedCategory                 string `json:"relatedCategory"`
	} `json:"planLevelLimitInfo"`
}

// CategoryLevelBenefit is one entry in categoryLevelBenefits (per providerType + category).
type CategoryLevelBenefit struct {
	ProviderType      string `json:"providerType"` // "I" or "O"
	ProcedureCategory struct {
		CodeValue string `json:"codeValue"`
		CodeDesc  string `json:"codeDesc"`
	} `json:"procedureCategory"`
	CoveredBenefits      string `json:"coveredBenefits"` // "Y" or "N"
	CoveragePct          string `json:"coveragePct"`
	MemberCoinsurancePct string `json:"memberCoinsurancePct"`
	CopayAmt             string `json:"copayAmt"`
	DeductibleApplies    string `json:"deductibleApplies"` // "Y" or "N"
	WaitingPeriodType    string `json:"waitingPeriodType"`
	WaitingPeriod        string `json:"waitingPeriod"`
	WaitingPeriodMetDate string `json:"waitingPeriodMetDate"`
	DeductibleType       string `json:"deductibleType"`
}

// UtilizationHistoryResponse is the parsed body of GET /apps/dental/utilizationHistory.
type UtilizationHistoryResponse struct {
	Result struct {
		DentalServiceHistory struct {
			MemberName struct {
				FirstName  string `json:"firstName"`
				MiddleName string `json:"middleName"`
				LastName   string `json:"lastName"`
			} `json:"memberName"`
			MemberRelationship string         `json:"memberRelationship"`
			Procedures         []UHCProcedure `json:"procedures"`
		} `json:"dentalServiceHistory"`
	} `json:"result"`
}

// UHCProcedure is one CDT code entry from utilizationHistory.
type UHCProcedure struct {
	Procedure struct {
		CodeValue string `json:"codeValue"`
		CodeDesc  string `json:"codeDesc"`
	} `json:"procedure"`
	ProcedureCategory     string       `json:"procedureCategory"`
	EHBIndicator          string       `json:"ehbIndicator"`
	InNetworkFrequency    string       `json:"inNetworkFrequency"`
	OutOfNetworkFrequency string       `json:"outOfNetworkFrequency"`
	AgeLimit              string       `json:"ageLimit"`
	AlternateBenefit      string       `json:"alternateBenefit"`
	RelatedCode           string       `json:"relatedCode"`
	Services              []UHCService `json:"services"`
}

// UHCService is a prior-service record within a UHCProcedure.
type UHCService struct {
	ServiceDate  string `json:"serviceDate"`
	ToothRange   string `json:"toothRange"`
	ToothSurface string `json:"toothSurface"`
}

// ── Probe ─────────────────────────────────────────────────────────────────────

// Probe executes UHC Dental API calls using a shared cookie jar seeded with
// provider session cookies from the browser login.
type Probe struct {
	client     *http.Client
	jar        *cookiejar.Jar
	baseURL    *url.URL
	providerID string
}

// NewProbe creates a Probe and injects session cookies from the browser login.
func NewProbe(sessionCookies SessionCookies) (*Probe, error) {
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, fmt.Errorf("create cookie jar: %w", err)
	}

	u, _ := url.Parse(baseURL)
	jar.SetCookies(u, sessionCookies.Raw)

	return &Probe{
		client:     &http.Client{Jar: jar, Timeout: 30 * time.Second},
		jar:        jar,
		baseURL:    u,
		providerID: sessionCookies.ProviderID,
	}, nil
}

// ParseMemberSearchBody parses the JSON body from a /apps/dental/member response.
// Used by the browser session so the parsing logic stays in the api package.
func ParseMemberSearchBody(body []byte) (*MemberInfo, error) {
	return ParseMemberSearchBodyForDOB(body, "")
}

// ParseMemberSearchBodyForDOB parses the member search body and prefers the
// consumer whose birth date matches the DOB submitted to the search form.
func ParseMemberSearchBodyForDOB(body []byte, dob string) (*MemberInfo, error) {
	var parsed memberSearchResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("decode member search: %w", err)
	}
	if len(parsed.Result.Consumers) == 0 {
		return nil, fmt.Errorf("member not found (empty consumers)")
	}
	consumerIndex := 0
	if targetDOB := normalizeMemberSearchDOB(dob); targetDOB != "" {
		for i, c := range parsed.Result.Consumers {
			if normalizeMemberSearchDOB(c.Demographics.BirthDate) == targetDOB {
				consumerIndex = i
				break
			}
		}
	}
	c := parsed.Result.Consumers[consumerIndex]
	stopDate := ""
	if len(c.Coverages) > 0 {
		stopDate = c.Coverages[0].CoverageDates.StopDate
	}
	mi := &MemberInfo{
		MemberContrivedKey: c.Demographics.ConsumerID,
		SubscriberID:       c.Demographics.SubscriberID,
		MemberFirstName:    c.Demographics.MemberName.FirstName,
		MemberLastName:     c.Demographics.MemberName.LastName,
		BirthDate:          c.Demographics.BirthDate,
		GroupID:            c.GroupInfo.GroupID,
		GroupName:          c.GroupInfo.GroupName,
		CoverageStopDate:   stopDate,
	}
	return mi, nil
}

func normalizeMemberSearchDOB(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	for _, layout := range []string{"2006-01-02", "01/02/2006", "1/2/2006", "01-02-2006", "1-2-2006"} {
		if t, err := time.Parse(layout, value); err == nil {
			return t.Format("2006-01-02")
		}
	}
	return value
}

// SearchMember POSTs to /apps/dental/member and parses the JSON response body.
func (p *Probe) SearchMember(ctx context.Context, subscriberID, dob, serviceDate string) (*MemberInfo, error) {
	body := "applicationId=DBP" +
		"&dateOfBirth=" + dob +
		"&roleId=DBP" +
		"&maximumConsumerRecordCount=50" +
		"&coverageTypeCode=37" +
		"&timelineIndicator=2" +
		"&sourceCodeIndicator=1" +
		"&asOfDate=" + serviceDate +
		"&familyIndicator=I" +
		"&searchId=" + subscriberID

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		baseURL+"/apps/dental/member", strings.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/xml;")
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("csrf-token", "undefined")
	req.Header.Set("Referer", baseURL+"/content/dental-benefits-provider/en/secure/search-landing.html")

	resp, err := p.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("member search request: %w", err)
	}
	defer resp.Body.Close()
	bodyBytes, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK {
		log.Printf("[UHCDental] member search HTTP %d body: %s", resp.StatusCode, string(bodyBytes))
		return nil, fmt.Errorf("member search HTTP %d", resp.StatusCode)
	}

	mi, err := ParseMemberSearchBodyForDOB(bodyBytes, dob)
	if err != nil {
		return nil, fmt.Errorf("member not found (subscriberID=%s): %w", subscriberID, err)
	}
	return mi, nil
}

// FetchBenefitSummary calls GET /apps/dental/benefitsummary for the current member.
// serviceDate must be MM/DD/YYYY; effectiveDate is sent as YYYY/MM/DD per portal.
func (p *Probe) FetchBenefitSummary(ctx context.Context, planID, memberContrivedKey, serviceDate string) (*BenefitSummaryResponse, error) {
	// Portal sends effectiveDate as YYYY/MM/DD.
	effectiveDate := serviceDate
	if t, err := time.Parse("01/02/2006", serviceDate); err == nil {
		effectiveDate = t.Format("2006/01/02")
	}
	params := url.Values{
		"productId":           {planID},
		"effectiveDate":       {effectiveDate},
		"providerType":        {"*"},
		"memberContrivedKey":  {memberContrivedKey},
		"accumulatorNumber":   {"0"},
		"categoryId":          {"*"},
		"procedureCode":       {"*"},
		"descriptionRequired": {"Y"},
	}
	endpoint := baseURL + "/apps/dental/benefitsummary?" + params.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json, text/plain, */*")

	resp, err := p.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("benefitsummary request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("benefitsummary HTTP %d", resp.StatusCode)
	}

	var result BenefitSummaryResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode benefitsummary: %w", err)
	}
	return &result, nil
}

// EligSummaryResponse is the parsed body of GET /apps/dental/eligsummary.
// Full result is captured as raw JSON for logging; ProductID is extracted directly.
type EligSummaryResponse struct {
	Result struct {
		EligibilitySummary struct {
			ProductID string `json:"productId"`
		} `json:"eligibilitySummary"`
		Raw json.RawMessage `json:"-"`
	} `json:"result"`
	RawBody string `json:"-"`
}

// FetchEligSummary calls GET /apps/dental/eligsummary. The portal calls this
// immediately after the member search; it likely sets server-side state required
// before benefitsummary and utilizationHistory will succeed.
func (p *Probe) FetchEligSummary(ctx context.Context, memberContrivedKey, serviceDate string) (*EligSummaryResponse, error) {
	// Portal sends startDate/stopDate as YYYY-MM-DD.
	isoDate := serviceDate
	if t, err := time.Parse("01/02/2006", serviceDate); err == nil {
		isoDate = t.Format("2006-01-02")
	}
	params := url.Values{
		"memberContrivedKey":  {memberContrivedKey},
		"facetsIdentity":      {"FXIGUESTP"},
		"startDate":           {isoDate},
		"stopDate":            {isoDate},
		"requestType":         {"P"},
		"lapAndHcrInfoNeeded": {"Y"},
		"providerId":          {p.providerID},
	}
	endpoint := baseURL + "/apps/dental/eligsummary"

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(params.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded; charset=UTF-8")
	req.Header.Set("Referer", baseURL+"/content/dental-benefits-provider/en/secure/eligibility-summary.html")

	resp, err := p.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("eligsummary request: %w", err)
	}
	defer resp.Body.Close()

	bodyBytes, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		log.Printf("[UHCDental] eligsummary HTTP %d body: %s", resp.StatusCode, string(bodyBytes))
		return nil, fmt.Errorf("eligsummary HTTP %d", resp.StatusCode)
	}

	var result EligSummaryResponse
	if err := json.Unmarshal(bodyBytes, &result); err != nil {
		return nil, fmt.Errorf("decode eligsummary: %w", err)
	}
	return &result, nil
}

// FetchUtilizationHistory calls GET /apps/dental/utilizationHistory for the current member.
func (p *Probe) FetchUtilizationHistory(ctx context.Context, memberContrivedKey, planID string) (*UtilizationHistoryResponse, error) {
	now := time.Now()
	toDate := now.Format("2006-01-02")
	fromDate := now.AddDate(-5, 0, 0).Format("2006-01-02")

	params := url.Values{
		"facetsIdentity":     {"FXIGUEST"},
		"memberContrivedKey": {memberContrivedKey},
		"fromDate":           {fromDate},
		"toDate":             {toDate},
		"productId":          {planID},
	}
	endpoint := baseURL + "/apps/dental/utilizationHistory?" + params.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json, text/plain, */*")

	resp, err := p.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("utilizationHistory request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("utilizationHistory HTTP %d", resp.StatusCode)
	}

	var result UtilizationHistoryResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode utilizationHistory: %w", err)
	}
	return &result, nil
}
