// Package enrichment re-exports the shared enrichment types for backwards
// compatibility with existing DentaQuest imports.
// New code should import insurance-benefit-agent-go/internal/enrichment directly.
package enrichment

import shared "insurance-benefit-agent-go/internal/enrichment"

type (
	Enrichment         = shared.Enrichment
	CoverageStatus     = shared.CoverageStatus
	PlanSnapshot       = shared.PlanSnapshot
	NetworkSnapshot    = shared.NetworkSnapshot
	FinancialSnapshot  = shared.FinancialSnapshot
	AccumulatorSummary = shared.AccumulatorSummary
	Alert              = shared.Alert
	ProcedureHistory   = shared.ProcedureHistory
	ProcedureSignal    = shared.ProcedureSignal
	Metadata           = shared.Metadata
)
