package enrichment

import (
	"testing"

	"insurance-benefit-agent-go/internal/eligibility"
)

func TestBuildProducesHighSignalFields(t *testing.T) {
	el := &eligibility.PatientEligibility{
		Patient: eligibility.PatientInfo{
			FullName:                 "Jaquis Lysius",
			MemberType:               "Dependent",
			DateOfBirth:              "2013-03-19",
			MemberID:                 "K6100380701",
			GroupNumber:              "CHP-01",
			MemberEligibility:        "Active",
			EligibilityEffectiveDate: "2025-09-01",
			EligibilityEndDate:       "2032-03-31",
			IsEligible:               true,
		},
		Plan: eligibility.PlanInfo{
			Carrier:    "DentaQuest",
			PlanName:   "NY Emblem Health CHP",
			GroupName:  "Children's Health Insurance Plan",
			PlanDesign: "CHP",
			Provisions: map[string]string{"Network": "In & Out of Network"},
		},
		NetworkInfo: eligibility.NetworkInfo{
			Type:        "In & Out of Network",
			DisplayName: "Same Benefits",
			Confidence:  1,
			Reason:      "Parsed from member info payload",
		},
		Accumulators: []eligibility.Accumulator{
			{
				AccumulatorID: "annual-maximum",
				Kind:          "maximum",
				Type:          "calendar",
				Amount:        1500,
				Used:          200,
				Remaining:     1300,
				AccumulatorTreatmentTypes: []eligibility.AccumulatorTreatmentType{
					{Name: "Preventive"},
				},
			},
		},
		OfficeSummary: []eligibility.OfficeSummaryNote{
			{Tone: "warn", Text: "Pre-auth required for selected procedures."},
		},
		TreatmentHistory: map[string][]eligibility.TreatmentHistoryEntry{
			"D0120": {
				{ServiceDate: "2025-10-01", ToothCode: ""},
			},
		},
	}

	got := Build(el)
	if got == nil {
		t.Fatalf("Build() returned nil")
	}
	if !got.Coverage.IsEligible {
		t.Fatalf("expected coverage eligible")
	}
	if got.Plan.MemberID != "K6100380701" {
		t.Fatalf("unexpected member id: %q", got.Plan.MemberID)
	}
	if len(got.Financial.Maximums) != 1 {
		t.Fatalf("expected 1 maximum, got %d", len(got.Financial.Maximums))
	}
	if len(got.Alerts) == 0 {
		t.Fatalf("expected alerts from office summary")
	}
	if len(got.ProcedureHistory) != 1 {
		t.Fatalf("expected treatment history summary, got %d", len(got.ProcedureHistory))
	}
}
