// Package advanced builds the unified per-patient eligibility report that
// combines general plan info, network data, accumulators, and per-procedure
// enrichment (coverage %, network range, frequency limits, risk, accumulator
// applicability) into one object suitable for PDF rendering and office display.
//
// Input:  *eligibility.PatientEligibility  (from any payer adapter)
//
//	officeCodes         []string      (from PatCon API)
//	treatmentPlanCodes  []string      (from appointment TreatmentPlanProcCodes)
//
// Output: *PatientEligibilityReport
package advanced

// PatientEligibilityReport is the single unified object produced per patient
// per appointment.  It is payer-agnostic — any adapter that produces a
// PatientEligibility can be fed through advanced.Build().
type PatientEligibilityReport struct {
	Patient       PatientSnapshot      `json:"patient"`
	Plan          PlanSnapshot         `json:"plan"`
	Network       NetworkSnapshot      `json:"network"`
	MatrixColumns []MatrixColumn       `json:"matrixColumns,omitempty"`
	Matrix        []MatrixRow          `json:"matrix,omitempty"`
	Deductibles   []AccumulatorSummary `json:"deductibles,omitempty"`
	Maximums      []AccumulatorSummary `json:"maximums,omitempty"`
	// Per-procedure enriched records (office codes + treatment plan codes, deduped).
	Codes       []AdvancedCode `json:"codes,omitempty"`
	GeneratedAt string         `json:"generatedAt"`
	Source      string         `json:"source"`
	// StatusReason explains status-only reports, such as Not Active or Not Found,
	// with the payer/source signal that led to the conclusion.
	StatusReason string `json:"statusReason,omitempty"`
	// StatusOnly is true when the report was built from a stub (not found,
	// not active, unable to determine).  The PDF renders only the patient/plan
	// card and status badge — no procedures, no matrix, no accumulators.
	StatusOnly bool `json:"statusOnly,omitempty"`
}

// PatientSnapshot holds the patient identity fields.
type PatientSnapshot struct {
	FullName    string `json:"fullName"`
	DateOfBirth string `json:"dateOfBirth,omitempty"`
	MemberID    string `json:"memberId,omitempty"`
	GroupNumber string `json:"groupNumber,omitempty"`
	MemberType  string `json:"memberType,omitempty"` // "Subscriber" or "Dependent"
	IsEligible  bool   `json:"isEligible"`
	StatusLabel string `json:"statusLabel,omitempty"`
}

// PlanSnapshot holds plan-level identity.
type PlanSnapshot struct {
	Carrier        string      `json:"carrier,omitempty"`
	PlanName       string      `json:"planName,omitempty"`
	GroupName      string      `json:"groupName,omitempty"`
	PlanDesign     string      `json:"planDesign,omitempty"`
	StateRegulated bool        `json:"stateRegulated,omitempty"`
	Provisions     []Provision `json:"provisions,omitempty"`
}

// NetworkSnapshot holds the resolved network classification.
type NetworkSnapshot struct {
	Type        string `json:"type,omitempty"`        // raw network code e.g. "##PPO"
	DisplayName string `json:"displayName,omitempty"` // e.g. "PPO"
	DefaultTier string `json:"defaultTier,omitempty"` // TierID used for matrix lookups
}

// MatrixColumn describes one network tier column in the coverage matrix.
type MatrixColumn struct {
	TierID      string `json:"tierId"`
	DisplayName string `json:"displayName"`
}

// Provision is a key/value plan provision entry (e.g. "Basis of Payment" → "...").
type Provision struct {
	Label string `json:"label"`
	Value string `json:"value"`
}

// MatrixRow holds per-category coverage ranges across all network tiers.
type MatrixRow struct {
	Category string            `json:"category"`
	Values   map[string]string `json:"values"` // tierID → e.g. "80%–100%"
}

// AccumulatorSummary is a display-ready accumulator record.
type AccumulatorSummary struct {
	ID        string   `json:"id"`
	Name      string   `json:"name"`
	Services  []string `json:"services,omitempty"`
	Note      string   `json:"note,omitempty"`
	Kind      string   `json:"kind"`  // "deductible" or "maximum"
	Type      string   `json:"type"`  // "calendar" or "lifetime"
	Scope     string   `json:"scope"` // "individual", "family", or ""
	Amount    float64  `json:"amount"`
	Used      float64  `json:"used"`
	Remaining float64  `json:"remaining"`
	IsMet     bool     `json:"isMet"`
}

// AdvancedCode is a fully-enriched view of one CDT procedure code.
type AdvancedCode struct {
	Code        string `json:"code"`
	Category    string `json:"category,omitempty"`
	Description string `json:"description,omitempty"`

	// TP is true when this code came from the appointment's treatment plan.
	TP bool `json:"tp"`
	// InOfficeList is true when this code is in the office's standing code list.
	InOfficeList bool `json:"inOfficeList"`

	// Coverage and network.
	CoveragePercent int    `json:"coveragePercent"` // -1 = not in coverage data
	NetworkRange    string `json:"networkRange,omitempty"`

	// Financial flags from the default network's coverageDetail.
	PreApprovalRequired bool   `json:"preApprovalRequired,omitempty"`
	DeductibleExempted  bool   `json:"deductibleExempted,omitempty"`
	MaximumExempted     bool   `json:"maximumExempted,omitempty"`
	CopayAmount         string `json:"copayAmount,omitempty"`

	// Related codes that share the same frequency counter.
	CrossCheckCodes []string `json:"crossCheckCodes,omitempty"`

	// Parsed limitation text entries.
	Limitations []string       `json:"limitations,omitempty"`
	Frequency   *CodeFrequency `json:"frequency,omitempty"`

	// Risk assessment.
	Risk CodeRisk `json:"risk"`

	// Accumulators that apply to this code with remaining amounts.
	Accumulators []CodeAccumulator `json:"accumulators,omitempty"`

	// Flags.
	AgeIneligible bool `json:"ageIneligible,omitempty"`
	NotCovered    bool `json:"notCovered,omitempty"`

	// Prior service dates for this code from treatment history.
	TreatmentHistory []HistoryEntry `json:"treatmentHistory,omitempty"`
}

// CodeFrequency holds a parsed frequency limit and computed usage from history.
type CodeFrequency struct {
	// Parsed from limitation text.
	Allowed      *int   `json:"allowed"`      // max occurrences in period; nil = no numeric limit stated
	Period       string `json:"period"`       // human-readable: "1 year", "24 months", "lifetime"
	PeriodType   string `json:"periodType"`   // "calendar", "lifetime", "rolling"
	PeriodMonths int    `json:"periodMonths"` // months for rolling window; 12 for calendar; 0 for lifetime
	Scope        string `json:"scope"`        // "per patient", "per tooth", "per quadrant"
	Summary      string `json:"summary"`      // e.g. "once per calendar year per patient"

	// Networks this frequency limit applies to (e.g. "##NP,##PMR,##PPO").
	NetworksApplicable string `json:"networksApplicable,omitempty"`
	// CounterIdentifier links codes that share a frequency counter (e.g. "REGEXAMS").
	CounterIdentifier string `json:"counterIdentifier,omitempty"`

	// Computed from treatment history cross-reference.
	Used            int    `json:"used"`
	Remaining       int    `json:"remaining"`
	Exceeded        bool   `json:"exceeded"`
	UsageSummary    string `json:"usageSummary,omitempty"`    // e.g. "1 of 2 used; 1 remaining"
	LastServiceDate string `json:"lastServiceDate,omitempty"` // YYYY-MM-DD
	Message         string `json:"message,omitempty"`         // set when exceeded
}

// CodeRisk is the computed risk assessment for a procedure.
type CodeRisk struct {
	Level  string `json:"level"` // "ACTIVE", "RISKY", "DENIED", or "UNKNOWN"
	Reason string `json:"reason"`
	Detail string `json:"detail,omitempty"`
	Color  string `json:"color,omitempty"` // CSS hex: #27ae60 / #f59e0b / #e74c3c
}

// CodeAccumulator describes how one accumulator (deductible or maximum) applies
// to this procedure code.
type CodeAccumulator struct {
	ID        string  `json:"id"`
	Kind      string  `json:"kind"` // "deductible" or "maximum"
	Type      string  `json:"type"` // "calendar" or "lifetime"
	Amount    float64 `json:"amount"`
	Remaining float64 `json:"remaining"`
	Applies   bool    `json:"applies"`
	IsMet     bool    `json:"isMet"` // true when maximum Remaining == 0
}

// HistoryEntry is a prior-service record for this procedure code.
type HistoryEntry struct {
	ServiceDate      string `json:"serviceDate,omitempty"`
	ToothCode        string `json:"toothCode,omitempty"`
	ToothDescription string `json:"toothDescription,omitempty"`
	Surfaces         string `json:"surfaces,omitempty"`
}
