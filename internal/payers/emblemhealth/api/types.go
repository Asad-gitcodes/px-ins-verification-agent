package api

import (
	"encoding/json"
	"fmt"
	"strings"
)

const BulkMemberLimit = 25

type ProbeBundle struct {
	PayerURL          string                     `json:"payerUrl,omitempty"`
	RequestedMemberID string                     `json:"requestedMemberId"`
	Appointment       any                        `json:"appointment,omitempty"`
	Record            *MemberRecord              `json:"record,omitempty"`
	RawResult         *InvokeResult              `json:"rawResult,omitempty"`
	RecordedAt        string                     `json:"recordedAt,omitempty"`
	DetailData        map[string]json.RawMessage `json:"detailData,omitempty"`
}

type ApexRemoteResponse struct {
	StatusCode int    `json:"statusCode"`
	Type       string `json:"type"`
	TID        int    `json:"tid"`
	Ref        bool   `json:"ref"`
	Action     string `json:"action"`
	Method     string `json:"method"`
	Result     string `json:"result"`
}

// MemberRecords handles the Salesforce quirk where a single-record result is returned
// as an object {} instead of a one-element array [{}].
type MemberRecords []MemberRecord

func (r *MemberRecords) UnmarshalJSON(data []byte) error {
	data = []byte(strings.TrimSpace(string(data)))
	if len(data) == 0 || string(data) == "null" {
		return nil
	}
	if data[0] == '[' {
		var records []MemberRecord
		if err := json.Unmarshal(data, &records); err != nil {
			return err
		}
		*r = records
		return nil
	}
	var record MemberRecord
	if err := json.Unmarshal(data, &record); err != nil {
		return err
	}
	*r = MemberRecords{record}
	return nil
}

type InvokeResult struct {
	IPResult struct {
		EmptyResponse bool `json:"emptyResponse"`
		Response      struct {
			MemberList   *MemberRecord `json:"MemberList,omitempty"`
			Records      MemberRecords `json:"memberEligibilitySerachRecords,omitempty"`
			TotalRecords *struct {
				RecordCount int `json:"RecordCount"`
			} `json:"TotalRecords,omitempty"`
			Hits *struct {
				Total int `json:"total"`
			} `json:"hits,omitempty"`
		} `json:"response"`
		ErrorCode string `json:"errorCode"`
		Error     string `json:"error"`
	} `json:"IPResult"`
	ErrorCode string `json:"errorCode"`
	Error     string `json:"error"`
}

type MemberRecord struct {
	AltMemberID                        string `json:"altMemberId"`
	BirthDate                          string `json:"birthDate"`
	CoverageType                       string `json:"coverageType"`
	CoverageTypeIdentifier             string `json:"coverageTypeIdentifier"`
	EligibilityEffectiveDate           string `json:"eligibilityEffectiveDate"`
	EligibilityTerminationDate         string `json:"eligibilityTerminationDate"`
	Gender                             string `json:"gender"`
	MemberAltID                        string `json:"memberAltId"`
	MemberCombName                     string `json:"memberCombName"`
	MemberFirstName                    string `json:"memberFirstName"`
	MemberID                           string `json:"memberId"`
	MemberLastName                     string `json:"memberLastName"`
	MemberMiddleName                   string `json:"memberMiddleName"`
	MemberNMIAltID                     string `json:"memberNmiAltId"`
	MemberOriginalID                   string `json:"memberOriginalId"`
	OriginalBrand                      string `json:"originalBrand"`
	OriginalEligibilityEffectiveDate   string `json:"originalEligibilityEffectiveDate"`
	OriginalEligibilityTerminationDate string `json:"originalEligibilityTerminationDate"`
	PlanCategoryNum                    string `json:"planCategoryNum"`
	PlanCode                           string `json:"planCode"`
	PlanType                           string `json:"planType"`
	ProductType                        string `json:"productType"`
	Status                             string `json:"status"`
	SubscriberID                       string `json:"subscriberId"`
	TenantID                           string `json:"tenantId"`
	UserID                             string `json:"userId"`
}

func ParseApexResult(raw string) (*InvokeResult, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, fmt.Errorf("empty apex response")
	}
	var responses []ApexRemoteResponse
	if err := json.Unmarshal([]byte(raw), &responses); err != nil {
		return nil, fmt.Errorf("decode apex response: %w", err)
	}
	if len(responses) == 0 {
		return nil, fmt.Errorf("apex response was empty")
	}
	if responses[0].StatusCode < 200 || responses[0].StatusCode >= 300 {
		return nil, fmt.Errorf("apex statusCode=%d", responses[0].StatusCode)
	}
	var result InvokeResult
	if err := json.Unmarshal([]byte(responses[0].Result), &result); err != nil {
		return nil, fmt.Errorf("decode apex result: %w", err)
	}
	return &result, nil
}

func MatchRecordsByMemberID(result *InvokeResult) map[string]MemberRecord {
	out := map[string]MemberRecord{}
	if result == nil {
		return out
	}
	add := func(record MemberRecord) {
		for _, id := range recordIDs(record) {
			id = strings.ToUpper(strings.TrimSpace(id))
			if id == "" {
				continue
			}
			out[id] = record
		}
	}
	if result.IPResult.Response.MemberList != nil {
		add(*result.IPResult.Response.MemberList)
	}
	for _, record := range result.IPResult.Response.Records {
		add(record)
	}
	return out
}

func recordIDs(record MemberRecord) []string {
	return []string{
		record.MemberID,
		record.MemberAltID,
		record.AltMemberID,
		record.MemberOriginalID,
		record.MemberNMIAltID,
	}
}
