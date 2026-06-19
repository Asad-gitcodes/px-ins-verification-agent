package eligibility

import (
	"testing"

	"insurance-benefit-agent-go/internal/models"
	gapi "insurance-benefit-agent-go/internal/payers/guardian/api"
)

func TestBuildEligibilityFromProbeTreatsPastCoverageTermAsInactive(t *testing.T) {
	bundle := &gapi.ProbeBundle{
		Appointment: models.Appointment{
			AppointmentDate: "05-11-2026",
			SubscriberID:    "ILA347198",
		},
		SelectedMember: &gapi.SearchMember{
			FirstName:              "ELDON K",
			LastName:               "ROSCOE",
			Relationship:           "M",
			DateOfBirth:            "08/08/1961",
			GroupPolicyNumber:      "00490913",
			GroupName:              "CAMBRIDGE HEALTHCARE SERVICES",
			GRStatusCode:           "A",
			EffectiveDate:          "03/01/2025",
			MemberCoverageTermDate: "02/01/2026",
			GRTerminationDate:      "10/01/2013",
		},
		Member: &gapi.MemberResponse{
			TerminationDate: "02/01/2026",
		},
	}

	el := BuildEligibilityFromProbe(bundle)
	if el == nil {
		t.Fatal("BuildEligibilityFromProbe returned nil")
	}
	if el.Patient.IsEligible {
		t.Fatal("patient IsEligible=true, want false for coverage terminated before appointment")
	}
	if el.Patient.MemberEligibility != "Inactive" {
		t.Fatalf("MemberEligibility=%q, want Inactive", el.Patient.MemberEligibility)
	}
	if el.Patient.EligibilityEndDate != "2026-02-01" {
		t.Fatalf("EligibilityEndDate=%q, want 2026-02-01", el.Patient.EligibilityEndDate)
	}
}
