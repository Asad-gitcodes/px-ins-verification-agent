// Package eligibility maps the raw Delta Dental API bundle into the
// payer-agnostic PatientEligibility model.
package eligibility

import (
	"strings"
	"time"

	"insurance-benefit-agent-go/internal/eligibility"
	ddapi "insurance-benefit-agent-go/internal/payers/deltadentalins/api"
)

// BuildEligibilityFromProbeBundle converts a Delta Dental PatientAPIBundle
// (new portal: portal.deltadental.com) into the canonical PatientEligibility.
// Returns nil when the bundle has no usable member data.
func BuildEligibilityFromProbeBundle(bundle *ddapi.PatientAPIBundle) *eligibility.PatientEligibility {
	if bundle == nil || bundle.MemberSearch == nil {
		return nil
	}

	ms := bundle.MemberSearch
	appt := bundle.Appointment

	fullName := strings.TrimSpace(ms.SubscriberFirstName + " " + ms.SubscriberLastName)
	if fullName == "" {
		fullName = strings.TrimSpace(appt.FName + " " + appt.LName)
	}

	memberType := "Subscriber"
	if appt.Relationship != "" && !strings.EqualFold(appt.Relationship, "0") {
		memberType = "Dependent"
	}

	el := &eligibility.PatientEligibility{
		Patient: eligibility.PatientInfo{
			FullName:    fullName,
			MemberType:  memberType,
			DateOfBirth: ms.SubscriberDateOfBirth,
			MemberID:    strings.TrimSpace(appt.SubscriberID),
			GroupNumber: appt.GroupNum,
			IsEligible:  ms.ActiveStatus,
		},
		Plan: eligibility.PlanInfo{
			Carrier:    ms.MemberCompanyName,
			GroupName:  ms.GroupName,
			Provisions: make(map[string]string),
		},
	}

	// Benefits data (map[string]any) — extract what we can.
	// Expand this as you inspect real /benefits responses in DevTools.
	if bundle.Benefits != nil && bundle.Benefits.Raw != nil {
		applyRawBenefits(bundle.Benefits.Raw, el)
	}

	el.Metadata = eligibility.Metadata{
		EligibilityCheckedAt: time.Now().UTC().Format(time.RFC3339),
		Source:               "DeltaDentalAPIProbe",
	}
	return el
}

// applyRawBenefits reads the raw benefits map and populates coverage/accumulators.
// TODO: replace map lookups with typed fields once the /benefits response is mapped.
func applyRawBenefits(raw map[string]any, el *eligibility.PatientEligibility) {
	// Example — uncomment and adjust once you've captured a real /benefits response:
	//
	// if annualMax, ok := raw["annualMaximum"].(float64); ok {
	//     el.Accumulators = append(el.Accumulators, eligibility.Accumulator{
	//         Type:      "Annual Maximum",
	//         Network:   "In-Network",
	//         Period:    "Calendar Year",
	//         Limit:     annualMax,
	//         Remaining: annualMax,
	//     })
	// }
	//
	// if preventivePct, ok := raw["preventiveCoverage"].(float64); ok {
	//     el.Coverage.Categories = append(el.Coverage.Categories, eligibility.CoverageCategory{
	//         Name:           "Preventive",
	//         NetworkPercent: preventivePct,
	//     })
	// }

	_ = raw // suppress unused warning until fields are mapped
}
