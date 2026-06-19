// Package eligibility maps a Vyne Trellis ProbeBundle into the
// payer-agnostic PatientEligibility model.
package eligibility

import (
	"strings"
	"time"

	"insurance-benefit-agent-go/internal/eligibility"
	vtapi "insurance-benefit-agent-go/internal/payers/vynetrellis/api"
)

// BuildEligibilityFromProbe converts a Vyne Trellis ProbeBundle into the
// canonical PatientEligibility. Returns nil when the bundle is nil or has no patient.
func BuildEligibilityFromProbe(bundle *vtapi.ProbeBundle) *eligibility.PatientEligibility {
	if bundle == nil || bundle.Patient == nil {
		return nil
	}
	// A nil, empty, or non-definitive Verification should not be reported as Inactive.
	// Returning nil triggers BuildUnableToDetermineReport instead.
	if bundle.Verification == nil {
		return nil
	}
	if isIndeterminateVerification(bundle.Verification) {
		return nil
	}

	p := bundle.Patient
	fullName := strings.TrimSpace(p.PatientFirstName + " " + p.PatientLastName)
	subFullName := strings.TrimSpace(p.SubscriberFirstName + " " + p.SubscriberLastName)

	memberType := "Subscriber"
	if !p.PatientIsSub {
		memberType = "Dependent"
	}

	el := &eligibility.PatientEligibility{
		Patient: eligibility.PatientInfo{
			FullName:    fullName,
			DateOfBirth: normalizeDOB(p.PatientBirthdate),
			MemberID:    p.SubscriberId,
			MemberType:  memberType,
			IsEligible:  resolveIsEligible(bundle.Verification),
		},
		Plan: eligibility.PlanInfo{
			Carrier:    strings.TrimRight(strings.TrimSpace(p.CarrierName), ","),
			Provisions: make(map[string]string),
		},
		Coverage:      eligibility.Coverage{Categories: []eligibility.CoverageCategory{}},
		NetworkTiers:  []eligibility.NetworkTier{},
		NetworkMatrix: []eligibility.NetworkMatrixRow{},
		Accumulators:  []eligibility.Accumulator{},
		OfficeSummary: []eligibility.OfficeSummaryNote{},
	}

	if p.GroupNumber != "" {
		el.Patient.GroupNumber = p.GroupNumber
	}
	if subFullName != fullName && subFullName != "" {
		setProvision(el, "Subscriber", subFullName)
	}
	if p.SubscriberBirthdate != "" {
		setProvision(el, "Subscriber DOB", normalizeDOB(p.SubscriberBirthdate))
	}
	if p.IndividualNpi != "" {
		setProvision(el, "Provider NPI", p.IndividualNpi)
	}
	setProvision(el, "Carrier ID", p.CarrierId)

	if bundle.Verification != nil && bundle.Verification.HtmlResult != nil {
		enrichFromHTML(el, *bundle.Verification.HtmlResult)
	}

	el.Metadata = eligibility.Metadata{
		EligibilityCheckedAt: time.Now().UTC().Format(time.RFC3339),
		Source:               "VyneTrellisAPI",
	}
	return el
}

// isIndeterminateVerification returns true for responses that are system/data
// errors rather than a real eligibility answer, so they map to Unable to Determine.
func isIndeterminateVerification(v *vtapi.VerifyResponse) bool {
	if v.StatusCode == 0 && strings.TrimSpace(v.Status) == "" {
		return true // API returned no data
	}
	switch v.StatusCode {
	case 4:  // Unsupported Carrier
		return true
	case 20: // Insurance Info Issue (bad subscriber ID etc.)
		return true
	case 98: // Unknown
		return true
	}
	return false
}

func resolveIsEligible(v *vtapi.VerifyResponse) bool {
	if v == nil {
		return false
	}
	// StatusCode 1 = Eligible (confirmed from live API).
	// Fall back to Status string for forward-compatibility with unknown codes.
	if v.StatusCode == 1 {
		return true
	}
	s := strings.ToLower(strings.TrimSpace(v.Status))
	return s == "active" || s == "eligible" || strings.HasPrefix(s, "active")
}

func normalizeDOB(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	// Vyne returns "07/18/2015 12:00:00.000 AM" or "07/18/2015"
	if idx := strings.Index(value, " "); idx > 0 {
		value = value[:idx]
	}
	// Convert MM/DD/YYYY → YYYY-MM-DD
	layouts := []struct{ in, out string }{
		{"01/02/2006", "2006-01-02"},
		{"1/2/2006", "2006-01-02"},
	}
	for _, l := range layouts {
		if t, err := time.Parse(l.in, value); err == nil {
			return t.Format(l.out)
		}
	}
	return value
}

func setProvision(el *eligibility.PatientEligibility, key, value string) {
	v := strings.TrimSpace(value)
	if v == "" {
		return
	}
	if el.Plan.Provisions == nil {
		el.Plan.Provisions = make(map[string]string)
	}
	el.Plan.Provisions[key] = v
}
