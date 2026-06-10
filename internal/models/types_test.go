package models

import (
	"encoding/json"
	"testing"
)

func TestAppointmentUnmarshalInsuranceKeys(t *testing.T) {
	var appt Appointment
	if err := json.Unmarshal([]byte(`{
		"aptNum":121019,
		"patNum":6812,
		"InsSubNum":2979,
		"PlanNum":3047,
		"ordinal":1
	}`), &appt); err != nil {
		t.Fatal(err)
	}
	if appt.InsSubNum != "2979" {
		t.Fatalf("InsSubNum=%q", appt.InsSubNum)
	}
	if appt.PlanNum != "3047" {
		t.Fatalf("PlanNum=%q", appt.PlanNum)
	}
	if appt.Ordinal != "1" {
		t.Fatalf("Ordinal=%q", appt.Ordinal)
	}
}
