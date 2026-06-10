package eligibility

import (
	"strings"
	"time"

	elig "insurance-benefit-agent-go/internal/eligibility"
	dapi "insurance-benefit-agent-go/internal/payers/dentical/api"
)

func BuildEligibilityFromProbe(bundle *dapi.ProbeBundle) *elig.PatientEligibility {
	if bundle == nil || bundle.Response == nil || bundle.Response.Results == nil {
		return nil
	}
	res := bundle.Response.Results
	provisions := map[string]string{}
	addProvision := func(label, value string) {
		value = strings.TrimSpace(value)
		if value != "" {
			provisions[label] = value
		}
	}

	addProvision("EVC Trace Number", res.EVCTraceNumber)
	addProvision("County", res.EligibilityCodesForMonth.CountyCode)
	addProvision("Primary Aid", res.EligibilityCodesForMonth.PrimaryAid)
	addProvision("First Aid", res.EligibilityCodesForMonth.FirstAid)
	addProvision("Second Aid", res.EligibilityCodesForMonth.SecondAid)
	addProvision("Third Aid", res.EligibilityCodesForMonth.ThirdAid)
	addProvision("SOC Percent Obligation", res.PercentObligation)
	addProvision("SOC Total Obligation", res.TotalObligation)
	addProvision("SOC Remaining", res.RemainingSOC)
	addProvision("Service Type", res.ServiceType)
	addProvision("PCP Phone", res.PCPPhone)
	addProvision("Denti-Cal Message", res.TextMessage)
	addProvision("Transaction", res.EligTransPerfBy)

	return &elig.PatientEligibility{
		Patient: elig.PatientInfo{
			FullName:                 strings.TrimSpace(res.Name.FirstName + " " + res.Name.LastName),
			MemberType:               "Subscriber",
			DateOfBirth:              firstNonEmpty(res.BirthDate, bundle.Appointment.DOB, bundle.Appointment.SubDOB),
			MemberID:                 firstNonEmpty(res.SubscriberID, bundle.Appointment.SubscriberID),
			MemberEligibility:        eligibilityLabel(res.FoundElig),
			EligibilityEffectiveDate: res.ServiceDate,
			IsEligible:               strings.EqualFold(strings.TrimSpace(res.FoundElig), "Y"),
		},
		Plan: elig.PlanInfo{
			Carrier:    "Denti-Cal",
			PlanName:   "Medi-Cal Dental",
			Provisions: provisions,
		},
		Metadata: elig.Metadata{
			EligibilityCheckedAt: firstNonEmpty(bundle.RecordedAt, time.Now().UTC().Format(time.RFC3339)),
			Source:               "Denti-Cal GraphQL Eligibility",
		},
	}
}

func eligibilityLabel(foundElig string) string {
	if strings.EqualFold(strings.TrimSpace(foundElig), "Y") {
		return "Active"
	}
	return "Not Active"
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" {
			return value
		}
	}
	return ""
}
