package eligibility

import (
	"os"
	"testing"

	elig "insurance-benefit-agent-go/internal/eligibility"
)

func loadTestHTML(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Skipf("test HTML not found at %s: %v", path, err)
	}
	return string(data)
}

func newBlankEligibility(carrier string) *elig.PatientEligibility {
	return &elig.PatientEligibility{
		Plan:         elig.PlanInfo{Carrier: carrier, Provisions: make(map[string]string)},
		Coverage:     elig.Coverage{Categories: []elig.CoverageCategory{}},
		Accumulators: []elig.Accumulator{},
	}
}

func TestEnrichFromHTML_Ashley_Guardian(t *testing.T) {
	htmlContent := loadTestHTML(t, `C:\temp\ashley_stopka.html`)
	el := newBlankEligibility("Guardian Life Insurance Co")

	enrichFromHTML(el, htmlContent)

	// eligibility dates
	if el.Patient.EligibilityEffectiveDate != "2025-07-01" {
		t.Errorf("EligibilityEffectiveDate: got %q, want 2025-07-01", el.Patient.EligibilityEffectiveDate)
	}
	if el.Patient.EligibilityEndDate != "2050-12-31" {
		t.Errorf("EligibilityEndDate: got %q, want 2050-12-31", el.Patient.EligibilityEndDate)
	}

	// group info
	if el.Patient.GroupNumber != "00581822" {
		t.Errorf("GroupNumber: got %q, want 00581822", el.Patient.GroupNumber)
	}
	if el.Plan.GroupName != "PT SOLUTIONS HOLDINGS, LLC" {
		t.Errorf("GroupName: got %q, want PT SOLUTIONS HOLDINGS, LLC", el.Plan.GroupName)
	}

	// accumulators
	if len(el.Accumulators) == 0 {
		t.Fatal("expected accumulators, got none")
	}

	// find calendar year max for dental care individual in-network
	var cyMax, cyOON, orthoMax *elig.Accumulator
	for i := range el.Accumulators {
		a := &el.Accumulators[i]
		t.Logf("accumulator: id=%s name=%q kind=%s type=%s scope=%s amount=%.2f remaining=%.2f",
			a.AccumulatorID, a.Name, a.Kind, a.Type, a.Scope, a.Amount, a.Remaining)
		if a.Type == "calendar" && a.Scope == "individual" && !containsOON(a.AccumulatorID) && cyMax == nil {
			cyMax = a
		}
		if a.Type == "calendar" && a.Scope == "individual" && containsOON(a.AccumulatorID) && cyOON == nil {
			cyOON = a
		}
		if a.Type == "lifetime" && a.Scope == "individual" && !containsOON(a.AccumulatorID) && orthoMax == nil {
			orthoMax = a
		}
	}

	if cyMax == nil {
		t.Error("missing calendar year individual in-network accumulator")
	} else {
		if cyMax.Amount != 1750 {
			t.Errorf("calendar year max: got %.2f, want 1750.00", cyMax.Amount)
		}
		if cyMax.Remaining != 1590 {
			t.Errorf("calendar year remaining: got %.2f, want 1590.00", cyMax.Remaining)
		}
		if cyMax.Used != 160 {
			t.Errorf("calendar year used: got %.2f, want 160.00", cyMax.Used)
		}
	}

	if cyOON == nil {
		t.Error("missing calendar year individual OON accumulator")
	} else if cyOON.Amount != 1750 {
		t.Errorf("OON calendar year max: got %.2f, want 1750.00", cyOON.Amount)
	}

	if orthoMax == nil {
		t.Error("missing lifetime ortho individual in-network accumulator")
	} else if orthoMax.Amount != 1500 {
		t.Errorf("lifetime ortho max: got %.2f, want 1500.00", orthoMax.Amount)
	}
}

func TestEnrichFromHTML_Teoman_DeltaDental(t *testing.T) {
	htmlContent := loadTestHTML(t, `C:\temp\teoman_sener.html`)
	el := newBlankEligibility("Delta Dental of California")

	enrichFromHTML(el, htmlContent)

	// group info
	if el.Patient.GroupNumber != "18935-00001" {
		t.Errorf("GroupNumber: got %q, want 18935-00001", el.Patient.GroupNumber)
	}
	if el.Plan.GroupName != "ServiceNow, Inc." {
		t.Errorf("GroupName: got %q, want ServiceNow, Inc.", el.Plan.GroupName)
	}

	// plan name from Active Coverage DESCRIPTION
	if el.Plan.PlanName != "Delta Dental PPO" {
		t.Errorf("PlanName: got %q, want Delta Dental PPO", el.Plan.PlanName)
	}

	// eligibility dates (from Dependent Eligibility Dates section)
	if el.Patient.EligibilityEffectiveDate == "" {
		t.Error("EligibilityEffectiveDate should not be empty")
	}

	// accumulators
	if len(el.Accumulators) == 0 {
		t.Fatal("expected accumulators, got none")
	}
	for _, a := range el.Accumulators {
		t.Logf("accumulator: id=%s name=%q amount=%.2f remaining=%.2f",
			a.AccumulatorID, a.Name, a.Amount, a.Remaining)
	}

	var cyMax *elig.Accumulator
	for i := range el.Accumulators {
		a := &el.Accumulators[i]
		if a.Type == "calendar" && a.Scope == "individual" && !containsOON(a.AccumulatorID) && cyMax == nil {
			cyMax = a
		}
	}
	if cyMax == nil {
		t.Error("missing calendar year individual in-network accumulator")
	} else if cyMax.Amount != 2500 {
		t.Errorf("calendar year max: got %.2f, want 2500.00", cyMax.Amount)
	}
}

func containsOON(id string) bool {
	return len(id) > 3 && id[len(id)-3:] == "out"
}
