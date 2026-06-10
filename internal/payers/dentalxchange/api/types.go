package api

import "insurance-benefit-agent-go/internal/models"

type ProbeBundle struct {
	Appointment     models.Appointment `json:"appointment"`
	PayerURL        string             `json:"payerUrl"`
	RecordedAt      string             `json:"recordedAt"`
	SearchRequest   SearchRequest      `json:"searchRequest"`
	EligibilityPage PageSnapshot       `json:"eligibilityPage"`
	BenefitsPage    PageSnapshot       `json:"benefitsPage"`
	Error           string             `json:"error,omitempty"`
}

type SearchRequest struct {
	BillingProvider string `json:"billingProvider,omitempty"`
	ProviderText    string `json:"providerText,omitempty"`
	PayerValue      string `json:"payerValue,omitempty"`
	PayerLabel      string `json:"payerLabel,omitempty"`
	MemberID        string `json:"memberId,omitempty"`
	PatientName     string `json:"patientName,omitempty"`
	DateOfBirth     string `json:"dateOfBirth,omitempty"`
	GroupNumber     string `json:"groupNumber,omitempty"`
	Relationship    string `json:"relationship,omitempty"`
}

type PageSnapshot struct {
	URL       string `json:"url,omitempty"`
	Title     string `json:"title,omitempty"`
	HTML      string `json:"html,omitempty"`
	Text      string `json:"text,omitempty"`
	Status    string `json:"status,omitempty"`
	Location  string `json:"location,omitempty"`
	Bytes     int    `json:"bytes,omitempty"`
	FetchStep string `json:"fetchStep,omitempty"`
}
