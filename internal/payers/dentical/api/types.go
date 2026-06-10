package api

import "insurance-benefit-agent-go/internal/models"

type ProbeBundle struct {
	PayerURL    string             `json:"payerUrl,omitempty"`
	Appointment models.Appointment `json:"appointment"`
	Profile     *Profile           `json:"profile,omitempty"`
	Request     EligibilityRequest `json:"request"`
	Response    *EligibilityStatus `json:"response,omitempty"`
	RecordedAt  string             `json:"recordedAt"`
}

type Profile struct {
	UserID    string `json:"userId"`
	OrgID     string `json:"orgId"`
	LoggedIn  bool   `json:"loggedIn"`
	Role      string `json:"role,omitempty"`
	Persona   string `json:"persona,omitempty"`
	OrgType   string `json:"personaOrgType,omitempty"`
	FirstName string `json:"givenName,omitempty"`
	LastName  string `json:"familyName,omitempty"`
}

type EligibilityRequest struct {
	SubscriberID string `json:"subscriberID"`
	IssueDate    string `json:"issueDate"`
	BirthDate    string `json:"birthDate"`
	ServiceDate  string `json:"serviceDate"`
	ProviderID   string `json:"providerID"`
	OrgID        string `json:"orgID"`
	OrgUserID    string `json:"orgUserID"`
}

type EligibilityStatus struct {
	Status  string             `json:"status"`
	Results *EligibilityResult `json:"results,omitempty"`
	Errors  []EligibilityError `json:"errors,omitempty"`
}

type EligibilityResult struct {
	ErrorCode                int                      `json:"errorCode"`
	EVCTraceNumber           string                   `json:"evcTraceNumber"`
	ServiceDate              string                   `json:"serviceDate"`
	Name                     EligibilityName          `json:"name"`
	BirthDate                string                   `json:"birthDate"`
	FoundElig                string                   `json:"foundElig"`
	SubscriberID             string                   `json:"subscriberID"`
	SubmittedID              string                   `json:"submittedID"`
	MedicareID               string                   `json:"medicareID"`
	IssueDate                string                   `json:"issueDate"`
	PercentObligation        string                   `json:"percentObligation"`
	TotalObligation          string                   `json:"totalObligation"`
	RemainingSOC             string                   `json:"remainingSOC"`
	ServiceType              string                   `json:"serviceType"`
	SOCInfo                  []SOCInfo                `json:"socInfo"`
	EligTransPerfBy          string                   `json:"eligTransPerfBy"`
	EligibilityCode          int                      `json:"eligibilityCode"`
	PCPPhone                 string                   `json:"PCPPhone"`
	EligibilityCodesForMonth EligibilityCodesForMonth `json:"eligibilityCodesForMonth"`
	TextMessage              string                   `json:"textMessage"`
	TextMessageCode          int                      `json:"textMessageCode"`
}

type EligibilityName struct {
	LastName  string `json:"lastName"`
	FirstName string `json:"firstName"`
	Initial   string `json:"initial"`
}

type SOCInfo struct {
	CaseNum string `json:"caseNum"`
	Balance string `json:"balance"`
}

type EligibilityCodesForMonth struct {
	CountyCode string `json:"countyCode"`
	PrimaryAid string `json:"primaryAid"`
	FirstAid   string `json:"firstAid"`
	SecondAid  string `json:"secondAid"`
	ThirdAid   string `json:"thirdAid"`
}

type EligibilityError struct {
	SubscriberID string `json:"subscriberID"`
	IssueDate    string `json:"issueDate"`
	BirthDate    string `json:"birthDate"`
	ServiceDate  string `json:"serviceDate"`
}
