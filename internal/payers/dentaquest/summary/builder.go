package summary

import (
	"insurance-benefit-agent-go/internal/eligibility"
	"insurance-benefit-agent-go/internal/enrichment"
	"insurance-benefit-agent-go/internal/models"
	shared "insurance-benefit-agent-go/internal/summary"
)

// Build delegates to the shared summary builder.
func Build(appointment models.Appointment, el *eligibility.PatientEligibility, enriched *enrichment.Enrichment) *Document {
	return shared.Build(appointment, el, enriched)
}
