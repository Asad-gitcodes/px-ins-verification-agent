package payers

import (
	"fmt"
	"strings"
	"time"

	"insurance-benefit-agent-go/internal/advanced"
	"insurance-benefit-agent-go/internal/models"
)

// BuildNotFoundReport builds a status-only PatientEligibilityReport for when
// the payer portal returns no match for the patient.
func BuildNotFoundReport(appointment models.Appointment) *advanced.PatientEligibilityReport {
	r := buildStubReport(appointment, "Not Found")
	r.StatusReason = "No matching member was found in the payer response."
	return r
}

// BuildNotActiveReport builds a status-only PatientEligibilityReport for when
// coverage is found but the member is not currently active.  planName and
// carrierName may be empty if the caller has no plan data to show.
func BuildNotActiveReport(appointment models.Appointment, planName, carrierName, groupName string) *advanced.PatientEligibilityReport {
	r := buildStubReport(appointment, "Not Active")
	r.Plan.PlanName = planName
	r.Plan.Carrier = carrierName
	r.Plan.GroupName = groupName
	r.StatusReason = "Coverage was found, but the payer response indicated the member is not currently active."
	return r
}

// BuildActiveReport builds a status-only PatientEligibilityReport for when
// the member is confirmed active but no benefit detail is needed.
func BuildActiveReport(appointment models.Appointment, planName, carrierName, groupName string) *advanced.PatientEligibilityReport {
	r := buildStubReport(appointment, "Active")
	r.Patient.IsEligible = true
	r.Plan.PlanName = planName
	r.Plan.Carrier = carrierName
	r.Plan.GroupName = groupName
	return r
}

// BuildUnableToDetermineReport builds a status-only report for when a bundle
// was returned but eligibility could not be parsed from it.
func BuildUnableToDetermineReport(appointment models.Appointment) *advanced.PatientEligibilityReport {
	r := buildStubReport(appointment, "Unable to Determine")
	r.StatusReason = "The payer response could not be parsed into a definitive eligibility status."
	return r
}

// ApplyStatusProvenance ensures status-only reports show which carrier and
// payer method were attempted, even when the adapter only had enough data to
// build a Not Found / Not Active / Unable to Determine stub.
func ApplyStatusProvenance(report *advanced.PatientEligibilityReport, appointment models.Appointment, payerURL string) *advanced.PatientEligibilityReport {
	if report == nil || !report.StatusOnly {
		return report
	}
	if strings.TrimSpace(report.Plan.Carrier) == "" {
		report.Plan.Carrier = strings.TrimSpace(appointment.CarrierName)
	}
	if strings.TrimSpace(report.Plan.GroupName) == "" {
		report.Plan.GroupName = strings.TrimSpace(appointment.GroupName)
	}
	method := StatusSourceForPayerURL(payerURL)
	if strings.TrimSpace(report.Source) == "" || strings.HasPrefix(strings.TrimSpace(report.Source), "Stub:") {
		report.Source = method
	}
	if strings.TrimSpace(report.StatusReason) == "" {
		report.StatusReason = fmt.Sprintf("Tried %s for carrier %s.", method, firstNonEmpty(appointment.CarrierName, "unknown"))
	}
	return report
}

func StatusSourceForPayerURL(payerURL string) string {
	switch strings.ToLower(strings.TrimSpace(payerURL)) {
	case "dentalxchange.com":
		return "DentalXChangeClaimConnect"
	case "guardianlife.com", "guardiananytime.com", "www.guardiananytime.com":
		return "Guardian Anytime API"
	case "metlife.com":
		return "MetLifeAPIProbe"
	case "uhcdental.com":
		return "UHCDentalAPIProbe"
	case "deltadentalins.com":
		return "DeltaDentalAPIProbe"
	case "dentaquest.com":
		return "DentaQuestAPIProbe"
	case "denti-cal.com", "dentical.com":
		return "Denti-Cal GraphQL Eligibility"
	case "emblemhealth.com":
		return "EmblemHealthApexRemote"
	case "vynetrellis.com":
		return "VyneTrellisAPI"
	default:
		if strings.TrimSpace(payerURL) != "" {
			return strings.TrimSpace(payerURL)
		}
		return "Payer probe"
	}
}

func buildStubReport(appointment models.Appointment, status string) *advanced.PatientEligibilityReport {
	fullName := strings.TrimSpace(appointment.FName + " " + appointment.LName)
	if fullName == "" {
		fullName = strings.TrimSpace(appointment.SubFName + " " + appointment.SubLName)
	}

	memberType := "Subscriber"
	if strings.EqualFold(appointment.Relationship, "dependent") {
		memberType = "Dependent"
	}

	return &advanced.PatientEligibilityReport{
		Patient: advanced.PatientSnapshot{
			FullName:    fullName,
			DateOfBirth: appointment.DOB,
			MemberID:    appointment.SubscriberID,
			GroupNumber: appointment.GroupNum,
			MemberType:  memberType,
			IsEligible:  false,
			StatusLabel: status,
		},
		Plan: advanced.PlanSnapshot{
			Carrier:   appointment.CarrierName,
			GroupName: appointment.GroupName,
		},
		GeneratedAt: time.Now().UTC().Format("01/02/2006 3:04 PM") + " UTC",
		Source:      "Stub:" + status,
		StatusOnly:  true,
	}
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}
