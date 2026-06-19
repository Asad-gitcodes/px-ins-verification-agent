// Package eligibility re-exports the shared eligibility types for backwards
// compatibility with existing DentaQuest imports.
// New code should import insurance-benefit-agent-go/internal/eligibility directly.
package eligibility

import sharedeligibility "insurance-benefit-agent-go/internal/eligibility"

// Re-export all types so existing imports continue to compile unchanged.
type (
	PatientEligibility       = sharedeligibility.PatientEligibility
	PatientInfo              = sharedeligibility.PatientInfo
	PlanInfo                 = sharedeligibility.PlanInfo
	Coverage                 = sharedeligibility.Coverage
	CoverageCategory         = sharedeligibility.CoverageCategory
	CoverageService          = sharedeligibility.CoverageService
	NetworkTier              = sharedeligibility.NetworkTier
	NetworkMatrixRow         = sharedeligibility.NetworkMatrixRow
	NetworkInfo              = sharedeligibility.NetworkInfo
	Accumulator              = sharedeligibility.Accumulator
	AccumulatorPeriod        = sharedeligibility.AccumulatorPeriod
	AccumulatorTreatmentType = sharedeligibility.AccumulatorTreatmentType
	OfficeSummaryNote        = sharedeligibility.OfficeSummaryNote
	TreatmentHistoryEntry    = sharedeligibility.TreatmentHistoryEntry
	Metadata                 = sharedeligibility.Metadata
)
