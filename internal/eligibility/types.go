// Package eligibility defines the canonical payer-agnostic eligibility model.
// All payer adapters normalize their raw API/scrape output into PatientEligibility
// so that downstream consumers (PDF generation, OpenDental write-back, etc.) can
// work with a single, stable structure regardless of the source payer.
package eligibility

import "time"

// PatientInfo holds patient-level eligibility fields.
type PatientInfo struct {
	FullName                 string
	MemberType               string // "Subscriber" or "Dependent"
	DateOfBirth              string
	MemberID                 string
	GroupNumber              string
	MemberEligibility        string
	EligibilityEffectiveDate string
	EligibilityEndDate       string
	IsEligible               bool
}

// PlanInfo holds insurance plan-level fields.
type PlanInfo struct {
	Carrier        string
	PlanName       string
	GroupName      string
	PlanDesign     string
	StateRegulated bool
	Highlights     []string
	Provisions     map[string]string
}

// AccumulatorTreatmentType holds a treatment class and its associated procedure codes.
type AccumulatorTreatmentType struct {
	Name           string
	ProcedureCodes []string
}

// AccumulatorPeriod is the date range for a calendar-year accumulator.
type AccumulatorPeriod struct {
	From time.Time
	To   time.Time
}

// Accumulator represents a deductible or maximum benefit accumulator.
type Accumulator struct {
	AccumulatorID             string
	Name                      string // e.g. "Calendar Family Deductible" — raw benefitName from payer
	Kind                      string // "deductible" or "maximum"
	Type                      string // "calendar" or "lifetime"
	Scope                     string // "individual" or "family" — parsed from Name
	Note                      string // optional: "Shared with: ..." or "1 Visit per 36 Months"
	Period                    *AccumulatorPeriod
	Amount                    float64
	Used                      float64
	Remaining                 float64
	AccumulatorTreatmentTypes []AccumulatorTreatmentType
}

// NetworkTier represents an in-network or out-of-network tier.
type NetworkTier struct {
	TierID       string
	DisplayName  string
	IsContracted bool
}

// NetworkMatrixRow holds per-class coinsurance values across network tiers.
type NetworkMatrixRow struct {
	Name   string
	Values map[string]string // tierID → formatted string e.g. "D0100-D0999 = 100%"
}

// CoverageService is a single procedure-code entry within a coverage category.
type CoverageService struct {
	Code                     string
	Description              string
	CoveragePercent          int
	Limitations              string
	AgeLimits                string
	PreAuthorizationRequired bool
	DeductibleExempted       bool
	MaximumExempted          bool
	CopayAmount              string
	CrossCheckCodes          []string
	FrequencyCounterID       string // shared counter linking crosscheck codes
	FrequencyNetworks        string // networks the frequency limit applies to
}

// CoverageCategory groups procedure codes by treatment class.
type CoverageCategory struct {
	Name     string
	Services []CoverageService
}

// Coverage holds the full office-code coverage breakdown.
type Coverage struct {
	Categories []CoverageCategory
}

// NetworkInfo holds the resolved network identification.
type NetworkInfo struct {
	Type        string
	DisplayName string
	Confidence  int
	Reason      string
}

// OfficeSummaryNote is a human-readable note added to the office summary.
type OfficeSummaryNote struct {
	Tone string
	Text string
}

// TreatmentHistoryEntry is one service-date record for a procedure code.
type TreatmentHistoryEntry struct {
	ServiceDate      string
	ToothCode        string
	ToothDescription string
	Surfaces         string
}

// Metadata holds eligibility check provenance.
type Metadata struct {
	EligibilityCheckedAt string
	Source               string
}

// PatientEligibility is the canonical eligibility result for one patient.
type PatientEligibility struct {
	Patient          PatientInfo
	Plan             PlanInfo
	Coverage         Coverage
	NetworkTiers     []NetworkTier
	NetworkMatrix    []NetworkMatrixRow
	NetworkInfo      NetworkInfo
	Metadata         Metadata
	Accumulators     []Accumulator
	OfficeSummary    []OfficeSummaryNote
	TreatmentHistory map[string][]TreatmentHistoryEntry // procedureCode → entries
}
