// Package eligibility maps raw NewPayer probe data → the canonical
// eligibility.PatientEligibility model used by the rest of the agent.
//
// HOW TO FILL THIS IN:
//  1. Run a probe for a real patient and look at the RawProbeData struct
//     written to artifacts/_probe_bucket/*.json.
//  2. Map each JSON field to the corresponding field in PatientEligibility.
//  3. The advanced.Build() call in adapter.go will handle all the benefit
//     percentage / deductible maths once the eligibility model is populated.
//
// KEY TYPES (all in internal/eligibility/types.go):
//   PatientEligibility.Patient   — member ID, DOB, name, IsEligible flag
//   PatientEligibility.Plan      — plan name, group, carrier, effective date
//   PatientEligibility.Coverage  — Categories slice (preventive/basic/major/ortho)
//   PatientEligibility.Accumulators — deductible/max used/remaining

package eligibility

import "insurance-benefit-agent-go/internal/eligibility"

// Build converts raw scraped data into the canonical PatientEligibility model.
// Returns nil when the data is too incomplete to build a report.
//
// TODO(newpayer): Replace every placeholder assignment with real field mappings.
func Build(raw any) *eligibility.PatientEligibility {
	if raw == nil {
		return nil
	}

	// TODO(newpayer): type-assert or JSON-unmarshal `raw` into your RawProbeData type.
	// Example (if RawProbeData is defined in the parent package and passed as *RawProbeData):
	//   data, ok := raw.(*newpayer.RawProbeData)
	//   if !ok || data == nil { return nil }

	el := &eligibility.PatientEligibility{}

	// ── Patient ───────────────────────────────────────────────────────────────
	// TODO(newpayer): populate from scraped fields.
	// el.Patient.MemberID      = data.MemberID
	// el.Patient.FullName      = data.MemberName
	// el.Patient.DateOfBirth   = data.DOB
	// el.Patient.IsEligible    = data.EligibilityStatus == "Active"
	// el.Patient.GroupNumber   = data.GroupNumber

	// ── Plan ─────────────────────────────────────────────────────────────────
	// TODO(newpayer): populate from scraped fields.
	// el.Plan.PlanName         = data.PlanName
	// el.Plan.Carrier          = data.CarrierName
	// el.Plan.GroupName        = data.GroupName
	// el.Plan.EffectiveDate    = data.EffectiveDate
	// el.Plan.TerminationDate  = data.TermDate

	// ── Coverage categories ───────────────────────────────────────────────────
	// TODO(newpayer): build el.Coverage.Categories from the benefit percentages.
	// Each category needs a Name and a NetworkPercent (in-network reimbursement %).
	// Standard category names: "Preventive", "Basic", "Major", "Orthodontic"
	// Example:
	//   el.Coverage.Categories = []eligibility.CoverageCategory{
	//       {Name: "Preventive", NetworkPercent: 100, NonNetworkPercent: 80},
	//       {Name: "Basic",      NetworkPercent: 80,  NonNetworkPercent: 50},
	//       {Name: "Major",      NetworkPercent: 50,  NonNetworkPercent: 50},
	//   }

	// ── Accumulators (deductible / annual max) ────────────────────────────────
	// TODO(newpayer): populate from scraped deductible / maximum fields.
	// el.Accumulators = []eligibility.Accumulator{
	//     {
	//         Type:         "Deductible",
	//         Network:      "In-Network",
	//         Period:       "Calendar Year",
	//         Limit:        50.00,
	//         UsedAmount:   0.00,
	//         Remaining:    50.00,
	//     },
	//     {
	//         Type:         "Annual Maximum",
	//         Network:      "In-Network",
	//         Period:       "Calendar Year",
	//         Limit:        1500.00,
	//         UsedAmount:   0.00,
	//         Remaining:    1500.00,
	//     },
	// }

	// Guard: if the patient is not flagged as eligible and we have no plan name,
	// the data is probably empty — return nil so the caller records "Unable to Determine".
	if !el.Patient.IsEligible && el.Plan.PlanName == "" {
		return nil
	}

	return el
}
