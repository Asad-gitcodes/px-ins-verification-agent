package eligibility

import (
	"testing"

	"insurance-benefit-agent-go/internal/models"
	dapi "insurance-benefit-agent-go/internal/payers/dentical/api"
)

func TestBuildEligibilityFromProbeMapsBasicDentiCalFields(t *testing.T) {
	bundle := &dapi.ProbeBundle{
		Appointment: models.Appointment{
			SubscriberID: "91765803G",
			DOB:          "12/18/1960",
		},
		RecordedAt: "2026-05-07T21:00:00Z",
		Response: &dapi.EligibilityStatus{
			Status: "200",
			Results: &dapi.EligibilityResult{
				EVCTraceNumber: "4254W9B2ZH",
				ServiceDate:    "05/07/2026",
				Name: dapi.EligibilityName{
					FirstName: "CARLOS",
					LastName:  "GONGORA HERRERA",
				},
				BirthDate:    "12/18/1960",
				FoundElig:    "Y",
				SubscriberID: "91765803G",
				EligibilityCodesForMonth: dapi.EligibilityCodesForMonth{
					CountyCode: "56 - Ventura",
					PrimaryAid: "M1",
				},
				TextMessage:     "MEDI-CAL ELIGIBLE W/ NO SOC/SPEND DOWN.",
				EligTransPerfBy: "Eligibility transaction performed by 1093777617",
			},
		},
	}

	el := BuildEligibilityFromProbe(bundle)
	if el == nil {
		t.Fatal("expected eligibility")
	}
	if !el.Patient.IsEligible {
		t.Fatal("expected active eligibility")
	}
	if el.Patient.FullName != "CARLOS GONGORA HERRERA" {
		t.Fatalf("full name = %q", el.Patient.FullName)
	}
	if got := el.Plan.Provisions["EVC Trace Number"]; got != "4254W9B2ZH" {
		t.Fatalf("EVC provision = %q", got)
	}
	if got := el.Plan.Provisions["Primary Aid"]; got != "M1" {
		t.Fatalf("primary aid provision = %q", got)
	}
}
