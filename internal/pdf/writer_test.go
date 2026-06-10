package pdf

import (
	"testing"

	"insurance-benefit-agent-go/internal/advanced"
)

func TestDisplayAccumulatorsPrioritizesGeneralIndividualMaximum(t *testing.T) {
	accums := []advanced.AccumulatorSummary{
		{
			ID:        "CALTJMXN",
			Name:      "Calendar TMJ Maximum",
			Kind:      "maximum",
			Type:      "calendar",
			Scope:     "individual",
			Amount:    500,
			Remaining: 500,
		},
		{
			ID:        "RMX1CYLT",
			Name:      "Calendar Maximum",
			Kind:      "maximum",
			Type:      "calendar",
			Scope:     "individual",
			Amount:    2000,
			Used:      170.80,
			Remaining: 1829.20,
		},
		{
			ID:        "LFTORMXN",
			Name:      "Lifetime Orthodontic Maximum",
			Kind:      "maximum",
			Type:      "lifetime",
			Scope:     "individual",
			Amount:    1800,
			Remaining: 1800,
		},
	}

	got := displayAccumulators(accums)
	if len(got) != 3 {
		t.Fatalf("displayAccumulators returned %d rows, want 3", len(got))
	}
	if got[0].ID != "RMX1CYLT" {
		t.Fatalf("first accumulator ID=%q name=%q, want RMX1CYLT Calendar Maximum", got[0].ID, got[0].Name)
	}
}

func TestDisplayCodesUsesTreatmentPlanWhenPresent(t *testing.T) {
	codes := []advanced.AdvancedCode{
		{Code: "D0120", TP: true, CoveragePercent: 100},
		{Code: "D4381", InOfficeList: true, CoveragePercent: 0},
	}

	got := displayCodes(codes)
	if len(got) != 1 {
		t.Fatalf("displayCodes returned %d rows, want 1: %+v", len(got), got)
	}
	if got[0].Code != "D0120" {
		t.Fatalf("displayed code=%q, want D0120", got[0].Code)
	}
}

func TestDisplayCodesFallsBackWhenNoTreatmentPlan(t *testing.T) {
	codes := []advanced.AdvancedCode{
		{Code: "D4381", InOfficeList: true, CoveragePercent: 0},
	}

	got := displayCodes(codes)
	if len(got) != 1 || got[0].Code != "D4381" {
		t.Fatalf("displayCodes fallback=%+v, want D4381", got)
	}
}
