package payers

import (
	"testing"

	"insurance-benefit-agent-go/internal/models"
)

func TestApplyStatusProvenanceAddsCarrierAndMethod(t *testing.T) {
	appt := models.Appointment{
		CarrierName: "Metlife",
		GroupName:   "TRIMEDX HOLDINGS, LLC",
	}
	report := BuildNotFoundReport(appt)
	report.Plan.Carrier = ""
	report.Plan.GroupName = ""

	ApplyStatusProvenance(report, appt, "metlife.com")

	if report.Plan.Carrier != "Metlife" {
		t.Fatalf("carrier=%q, want Metlife", report.Plan.Carrier)
	}
	if report.Plan.GroupName != "TRIMEDX HOLDINGS, LLC" {
		t.Fatalf("groupName=%q, want TRIMEDX HOLDINGS, LLC", report.Plan.GroupName)
	}
	if report.Source != "MetLifeAPIProbe" {
		t.Fatalf("source=%q, want MetLifeAPIProbe", report.Source)
	}
}

func TestApplyStatusProvenanceKeepsSpecificSourceAndReason(t *testing.T) {
	appt := models.Appointment{CarrierName: "Guardian"}
	report := BuildNotActiveReport(appt, "Dental PPO", "Guardian", "")
	report.Source = "Guardian Anytime API"
	report.StatusReason = "Guardian returned coverage through 2026-02-01."

	ApplyStatusProvenance(report, appt, "GuardianLife.com")

	if report.Source != "Guardian Anytime API" {
		t.Fatalf("source=%q", report.Source)
	}
	if report.StatusReason != "Guardian returned coverage through 2026-02-01." {
		t.Fatalf("statusReason=%q", report.StatusReason)
	}
}

func TestProbeAppointmentSegmentUsesInsuranceIdentityWhenNoAppointment(t *testing.T) {
	got := ProbeAppointmentSegment(models.Appointment{
		PatNum:    "123",
		Ordinal:   "2",
		InsSubNum: "700",
		PlanNum:   "800",
	})
	want := "noappt_ord2_sub700_plan800"
	if got != want {
		t.Fatalf("ProbeAppointmentSegment=%q want %q", got, want)
	}
}
