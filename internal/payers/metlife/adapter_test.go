package metlife

import (
	"testing"

	"insurance-benefit-agent-go/internal/models"
	metlifeapi "insurance-benefit-agent-go/internal/payers/metlife/api"
)

func TestBuildOverviewRequestPrefersSubscriberDemographics(t *testing.T) {
	appointment := models.Appointment{
		SubscriberID: "603085562",
		SubLName:     "Bhandari",
		SubZip:       "92692-1234",
		LName:        "Other",
	}

	req := buildOverviewRequest(appointment)

	if req.EmployeeID != "603085562" {
		t.Fatalf("expected employeeId to be set, got %q", req.EmployeeID)
	}
	if req.PlanTypeCode != defaultPlanTypeCode {
		t.Fatalf("expected default plan type code %q, got %q", defaultPlanTypeCode, req.PlanTypeCode)
	}
	if req.LastName != "Bhandari" {
		t.Fatalf("expected subscriber last name, got %q", req.LastName)
	}
	if req.ZipCode != "92692" {
		t.Fatalf("expected normalized subscriber zip, got %q", req.ZipCode)
	}
}

func TestSelectCoveredPersonPrefersExactPatientMatch(t *testing.T) {
	appointment := models.Appointment{
		FName:        "Asha",
		LName:        "Bhandari",
		DOB:          "01-02-2010",
		SubscriberID: "603085562",
	}

	persons := []metlifeapi.CoveredPerson{
		{
			FirstName:      "Raj",
			LastName:       "Bhandari",
			DateOfBirth:    "1980-05-01",
			EmployeeID:     "603085562",
			CoverageStatus: "active",
		},
		{
			FirstName:      "Asha",
			LastName:       "Bhandari",
			DateOfBirth:    "2010-01-02",
			EmployeeID:     "603085562",
			CoverageStatus: "active",
		},
	}

	person := selectCoveredPerson(persons, appointment)
	if person == nil {
		t.Fatal("expected covered person match")
	}
	if person.FirstName != "Asha" {
		t.Fatalf("expected dependent match, got %q", person.FirstName)
	}
}

func TestMetlifeEmployeeIDFallsBackForDependents(t *testing.T) {
	appointment := models.Appointment{SubscriberID: "138153636"}
	person := &metlifeapi.CoveredPerson{
		ActualID:   "138153636",
		EmployeeID: "",
	}

	if got := metlifeEmployeeID(person, appointment); got != "138153636" {
		t.Fatalf("expected fallback employee id, got %q", got)
	}
}

func TestMetlifeEmployeeIDPrefersAppointmentSubscriberID(t *testing.T) {
	appointment := models.Appointment{SubscriberID: "from-appointment"}
	person := &metlifeapi.CoveredPerson{
		ActualID:   "from-metlife",
		EmployeeID: "from-covered-person",
	}

	if got := metlifeEmployeeID(person, appointment); got != "from-appointment" {
		t.Fatalf("expected appointment subscriber id, got %q", got)
	}
}

func TestShouldRetryOverviewWithoutZipOnZipMismatch(t *testing.T) {
	req := metlifeapi.EligibilityOverviewRequest{ZipCode: "92656"}
	overview := &metlifeapi.EligibilityOverviewResponse{
		ReasonMessage: "No Zip Code Matches found",
	}

	if !shouldRetryOverviewWithoutZip(overview, req) {
		t.Fatal("expected zip mismatch overview to retry without zip")
	}
}

func TestBuildReportFromSpoolNotFoundCarriesCarrierAndReason(t *testing.T) {
	spool := &probeSpool{
		Appointment: models.Appointment{
			FName:        "Edward",
			LName:        "Mendez",
			DOB:          "10-06-1962",
			SubscriberID: "567430672",
			GroupNum:     "314782",
			GroupName:    "TRIMEDX HOLDINGS, LLC",
			CarrierName:  "Metlife",
		},
		Overview: &metlifeapi.EligibilityOverviewResponse{
			ReasonMessage: "No Zip Code Matches found",
		},
	}

	report := buildReportFromSpool(spool, nil, nil, nil)
	if report == nil {
		t.Fatal("expected not-found report")
	}
	if report.Plan.Carrier != "Metlife" {
		t.Fatalf("carrier=%q, want Metlife", report.Plan.Carrier)
	}
	if report.Plan.GroupName != "TRIMEDX HOLDINGS, LLC" {
		t.Fatalf("groupName=%q, want TRIMEDX HOLDINGS, LLC", report.Plan.GroupName)
	}
	if report.Source != "MetLifeAPIProbe" {
		t.Fatalf("source=%q, want MetLifeAPIProbe", report.Source)
	}
	if report.StatusReason != "MetLife overview returned: No Zip Code Matches found." {
		t.Fatalf("statusReason=%q", report.StatusReason)
	}
}
