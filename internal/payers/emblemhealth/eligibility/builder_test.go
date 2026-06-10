package eligibility

import (
	"testing"

	"insurance-benefit-agent-go/internal/models"
	emapi "insurance-benefit-agent-go/internal/payers/emblemhealth/api"
)

func TestBuildEligibilityFromProbeActive(t *testing.T) {
	bundle := &emapi.ProbeBundle{
		RequestedMemberID: "K6146807701",
		Record: &emapi.MemberRecord{
			MemberID:                   "K6146807701",
			SubscriberID:               "K61468077",
			MemberFirstName:            "FRANCHESCA",
			MemberLastName:             "TAPIA",
			BirthDate:                  "04/27/2000",
			Status:                     "Active",
			ProductType:                "Commercial PPO",
			PlanType:                   "PPO",
			PlanCode:                   "DP010149",
			CoverageType:               "Dental",
			EligibilityEffectiveDate:   "05/13/2025",
			EligibilityTerminationDate: "12/31/9999",
			OriginalBrand:              "GHI",
		},
	}

	el := BuildEligibilityFromProbe(bundle)
	if el == nil {
		t.Fatal("eligibility is nil")
	}
	if !el.Patient.IsEligible {
		t.Fatal("IsEligible=false, want true")
	}
	if el.Patient.MemberID != "K6146807701" {
		t.Fatalf("MemberID=%q", el.Patient.MemberID)
	}
	if el.Patient.EligibilityEffectiveDate != "2025-05-13" {
		t.Fatalf("effective=%q", el.Patient.EligibilityEffectiveDate)
	}
	if el.Plan.Provisions["Plan Code"] != "DP010149" {
		t.Fatalf("Plan Code provision=%q", el.Plan.Provisions["Plan Code"])
	}
}

func TestBuildEligibilityFromProbeMarksMissingProcedureCoverageUnknown(t *testing.T) {
	bundle := &emapi.ProbeBundle{
		RequestedMemberID: "K6146807701",
		Appointment: models.Appointment{
			TreatmentPlanProcCodes: "D0120,D1110",
		},
		Record: &emapi.MemberRecord{
			MemberID:                   "K6146807701",
			MemberFirstName:            "FRANCHESCA",
			MemberLastName:             "TAPIA",
			Status:                     "Active",
			CoverageType:               "Dental",
			EligibilityEffectiveDate:   "05/13/2025",
			EligibilityTerminationDate: "12/31/9999",
			OriginalBrand:              "GHI",
		},
	}

	el := BuildEligibilityFromProbe(bundle)
	if el == nil {
		t.Fatal("eligibility is nil")
	}
	if len(el.Coverage.Categories) == 0 || len(el.Coverage.Categories[0].Services) == 0 {
		t.Fatal("expected coverage service placeholders")
	}
	for _, category := range el.Coverage.Categories {
		for _, svc := range category.Services {
			if svc.CoveragePercent != -1 {
				t.Fatalf("CoveragePercent for %s=%d, want -1 unknown", svc.Code, svc.CoveragePercent)
			}
		}
	}
	if len(el.OfficeSummary) == 0 {
		t.Fatal("expected unknown coverage office summary note")
	}
}
