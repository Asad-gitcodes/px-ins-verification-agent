package advanced

import (
	"testing"

	"insurance-benefit-agent-go/internal/eligibility"
)

func TestBuildKeepsMissingProcedureCoverageUnknown(t *testing.T) {
	el := &eligibility.PatientEligibility{
		Patient: eligibility.PatientInfo{
			FullName:          "Test Patient",
			MemberEligibility: "Active",
			IsEligible:        true,
		},
		Coverage: eligibility.Coverage{
			Categories: []eligibility.CoverageCategory{
				{
					Name: "Preventive",
					Services: []eligibility.CoverageService{
						{
							Code:            "D1110",
							Description:     "Adult prophylaxis",
							CoveragePercent: -1,
						},
					},
				},
			},
		},
	}

	report := Build(el, nil, []string{"D1110"})
	if report == nil || len(report.Codes) != 1 {
		t.Fatalf("expected one advanced code, got %#v", report)
	}
	code := report.Codes[0]
	if code.Risk.Level != "UNKNOWN" {
		t.Fatalf("risk=%q, want UNKNOWN", code.Risk.Level)
	}
	if code.NotCovered {
		t.Fatal("missing coverage must not set NotCovered")
	}
}

func TestBuildKeepsUnreturnedProcedureUnknownForAllPayers(t *testing.T) {
	el := &eligibility.PatientEligibility{
		Patient: eligibility.PatientInfo{
			FullName:          "Test Patient",
			MemberEligibility: "Active",
			IsEligible:        true,
		},
	}

	report := Build(el, nil, []string{"D9999"})
	if report == nil || len(report.Codes) != 1 {
		t.Fatalf("expected one advanced code, got %#v", report)
	}
	code := report.Codes[0]
	if code.CoveragePercent != -1 {
		t.Fatalf("coveragePercent=%d, want -1", code.CoveragePercent)
	}
	if code.Risk.Level != "UNKNOWN" {
		t.Fatalf("risk=%q, want UNKNOWN", code.Risk.Level)
	}
	if code.NotCovered {
		t.Fatal("unreturned procedure must not set NotCovered")
	}
}
