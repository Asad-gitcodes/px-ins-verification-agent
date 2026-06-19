package enrichment

import (
	"insurance-benefit-agent-go/internal/eligibility"
	shared "insurance-benefit-agent-go/internal/enrichment"
)

// Build delegates to the shared enrichment builder.
func Build(el *eligibility.PatientEligibility) *Enrichment {
	return shared.Build(el)
}
