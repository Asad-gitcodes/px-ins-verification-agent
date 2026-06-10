// Package summary re-exports the shared summary types for backwards
// compatibility with existing DentaQuest imports.
// New code should import insurance-benefit-agent-go/internal/summary directly.
package summary

import shared "insurance-benefit-agent-go/internal/summary"

type (
	Document  = shared.Document
	Patient   = shared.Patient
	Plan      = shared.Plan
	Visit     = shared.Visit
	Financial = shared.Financial
	Metadata  = shared.Metadata
)
