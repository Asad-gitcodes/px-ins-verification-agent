// Package summary builds a human-readable, display-ready summary document
// from a PatientEligibility and its Enrichment.  It is payer-agnostic.
package summary

type Document struct {
	Title      string    `json:"title"`
	Patient    Patient   `json:"patient"`
	Plan       Plan      `json:"plan"`
	Visit      Visit     `json:"visit"`
	Financial  Financial `json:"financial"`
	Highlights []string  `json:"highlights,omitempty"`
	Alerts     []string  `json:"alerts,omitempty"`
	NextSteps  []string  `json:"nextSteps,omitempty"`
	Metadata   Metadata  `json:"metadata"`
}

type Patient struct {
	FullName string `json:"fullName"`
	DOB      string `json:"dob,omitempty"`
	MemberID string `json:"memberId,omitempty"`
	Type     string `json:"type,omitempty"`
	Status   string `json:"status,omitempty"`
}

type Plan struct {
	Carrier     string `json:"carrier,omitempty"`
	PlanName    string `json:"planName,omitempty"`
	GroupName   string `json:"groupName,omitempty"`
	GroupNumber string `json:"groupNumber,omitempty"`
	Network     string `json:"network,omitempty"`
}

type Visit struct {
	AppointmentDate string   `json:"appointmentDate,omitempty"`
	ProcedureCodes  []string `json:"procedureCodes,omitempty"`
}

type Financial struct {
	DeductibleLines []string `json:"deductibleLines,omitempty"`
	MaximumLines    []string `json:"maximumLines,omitempty"`
}

type Metadata struct {
	VerifiedAt string `json:"verifiedAt,omitempty"`
	Source     string `json:"source,omitempty"`
}
