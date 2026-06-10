// Package enrichment derives a structured, display-ready view from a
// canonical PatientEligibility.  It is payer-agnostic — any adapter
// that produces a PatientEligibility can run it through this package.
package enrichment

type Enrichment struct {
	Coverage  CoverageStatus    `json:"coverage"`
	Plan      PlanSnapshot      `json:"plan"`
	Network   NetworkSnapshot   `json:"network"`
	Financial FinancialSnapshot `json:"financial"`

	Alerts           []Alert            `json:"alerts"`
	ProcedureHistory []ProcedureHistory `json:"procedureHistory"`
	ProcedureSignals []ProcedureSignal  `json:"procedureSignals"`
	SummaryFacts     []string           `json:"summaryFacts"`
	Metadata         Metadata           `json:"metadata"`
}

type CoverageStatus struct {
	IsEligible    bool   `json:"isEligible"`
	StatusLabel   string `json:"statusLabel"`
	MemberType    string `json:"memberType"`
	EffectiveDate string `json:"effectiveDate,omitempty"`
	EndDate       string `json:"endDate,omitempty"`
}

type PlanSnapshot struct {
	Carrier     string            `json:"carrier,omitempty"`
	PlanName    string            `json:"planName,omitempty"`
	GroupName   string            `json:"groupName,omitempty"`
	GroupNumber string            `json:"groupNumber,omitempty"`
	MemberID    string            `json:"memberId,omitempty"`
	PlanDesign  string            `json:"planDesign,omitempty"`
	Highlights  []string          `json:"highlights,omitempty"`
	Provisions  map[string]string `json:"provisions,omitempty"`
}

type NetworkSnapshot struct {
	Type         string   `json:"type,omitempty"`
	DisplayName  string   `json:"displayName,omitempty"`
	Confidence   int      `json:"confidence,omitempty"`
	Reason       string   `json:"reason,omitempty"`
	TierNames    []string `json:"tierNames,omitempty"`
	MatrixLabels []string `json:"matrixLabels,omitempty"`
}

type FinancialSnapshot struct {
	Deductibles []AccumulatorSummary `json:"deductibles,omitempty"`
	Maximums    []AccumulatorSummary `json:"maximums,omitempty"`
}

type AccumulatorSummary struct {
	Name      string   `json:"name,omitempty"`
	Type      string   `json:"type,omitempty"`
	Amount    float64  `json:"amount"`
	Used      float64  `json:"used"`
	Remaining float64  `json:"remaining"`
	AppliesTo []string `json:"appliesTo,omitempty"`
}

type Alert struct {
	Severity string `json:"severity"`
	Category string `json:"category"`
	Title    string `json:"title"`
	Detail   string `json:"detail"`
}

type ProcedureHistory struct {
	Code            string   `json:"code"`
	Count           int      `json:"count"`
	LastServiceDate string   `json:"lastServiceDate,omitempty"`
	ToothCodes      []string `json:"toothCodes,omitempty"`
}

type ProcedureSignal struct {
	Category                 string `json:"category"`
	Code                     string `json:"code,omitempty"`
	Description              string `json:"description,omitempty"`
	CoveragePercent          int    `json:"coveragePercent,omitempty"`
	HasHistory               bool   `json:"hasHistory"`
	HistoryCount             int    `json:"historyCount,omitempty"`
	LastServiceDate          string `json:"lastServiceDate,omitempty"`
	Limitations              string `json:"limitations,omitempty"`
	AgeLimits                string `json:"ageLimits,omitempty"`
	PreAuthorizationRequired bool   `json:"preAuthorizationRequired,omitempty"`
}

type Metadata struct {
	GeneratedAt string `json:"generatedAt"`
	Source      string `json:"source"`
}
