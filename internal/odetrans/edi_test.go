package odetrans

import (
	"strings"
	"testing"
	"time"

	"insurance-benefit-agent-go/internal/advanced"
	"insurance-benefit-agent-go/internal/models"
)

func TestBuildPairActiveIncludesRemainingAndDeductibleRows(t *testing.T) {
	report := &advanced.PatientEligibilityReport{
		Patient: advanced.PatientSnapshot{
			FullName:    "Juan Garcia",
			DateOfBirth: "1973-07-04",
			MemberID:    "910352859",
			GroupNumber: "1368128",
			IsEligible:  true,
			StatusLabel: "Active",
		},
		Plan: advanced.PlanSnapshot{Carrier: "United Health Care"},
		Maximums: []advanced.AccumulatorSummary{
			{Name: "Dental Care Individual Calendar Maximum", Kind: "maximum", Type: "calendar", Scope: "individual", Amount: 2000, Remaining: 717.60},
			{Name: "Orthodontics Individual Lifetime Maximum", Kind: "maximum", Type: "lifetime", Scope: "individual", Amount: 1000, Remaining: 1000},
			{
				Name:      "Diagnostic Dental Individual Lifetime Maximum",
				Kind:      "maximum",
				Type:      "lifetime",
				Scope:     "individual",
				Amount:    2000,
				Remaining: 703.50,
				Note:      "Shared with: Diagnostic Dental, Periodontics, Adjunctive Dental Services, Diagnostic Xray, Routine (preventive) Dental And Orthodontics",
			},
			{Name: "Dental Care Family Calendar Maximum", Kind: "maximum", Type: "calendar", Scope: "family", Amount: 0, Remaining: 0},
		},
		Deductibles: []advanced.AccumulatorSummary{
			{Name: "Dental Care Individual Calendar Deductible", Kind: "deductible", Type: "calendar", Scope: "individual", Amount: 50, Remaining: 0},
		},
		Network: advanced.NetworkSnapshot{DefaultTier: "in"},
		MatrixColumns: []advanced.MatrixColumn{
			{TierID: "in", DisplayName: "In-Network"},
		},
		Matrix: []advanced.MatrixRow{
			{Category: "Diagnostic Dental", Values: map[string]string{"in": "100%"}},
			{Category: "Oral Exams and X-Rays", Values: map[string]string{"in": "100%"}},
			{Category: "Root Canals", Values: map[string]string{"in": "100%"}},
			{Category: "Gum Treatment", Values: map[string]string{"in": "100%"}},
			{Category: "Restorative", Values: map[string]string{"in": "80%"}},
			{Category: "Major Restorative", Values: map[string]string{"in": "50%"}},
			{Category: "Tooth Extraction", Values: map[string]string{"in": "50%-100%"}},
			{Category: "Partial Dentures, Full Dentures", Values: map[string]string{"in": "60%-100%"}},
			{Category: "Diagnostic Lab", Values: map[string]string{"in": "100%"}},
			{Category: "Maxillofacial Prosthetics", Values: map[string]string{"in": "50%"}},
			{Category: "Dental Accident", Values: map[string]string{"in": "100%"}},
			{Category: "Anesthesia", Values: map[string]string{"in": "80%"}},
		},
	}

	pair := BuildPair(BuildInput{
		Appointment: models.Appointment{
			AptNum:       "121019",
			PatNum:       "6812",
			FName:        "Juan",
			LName:        "Garcia",
			DOB:          "07-04-1973",
			SubscriberID: "910352859",
			GroupNum:     "1368128",
			CarrierName:  "United Health Care",
			PayerID:      "52133",
			Ordinal:      "1",
			InsSubNum:    "2979",
			PlanNum:      "3047",
		},
		Report: report,
		Status: "Verified",
		Provider: ProviderIdentity{
			FirstName: "Rachna",
			LastName:  "Surana",
			TaxID:     "461277465",
			NPI:       "1912143538",
		},
		Practice: PracticeIdentity{
			Address: "30021 Alicia Parkway",
			City:    "Laguna Niguel",
			State:   "CA",
			Zip:     "92677",
		},
		Now: time.Date(2026, 5, 11, 12, 14, 0, 0, time.UTC),
	})

	if !strings.Contains(pair.Request270, "ST*270*0001~") {
		t.Fatalf("270 missing ST segment: %s", pair.Request270)
	}
	t.Logf("270Request: %s", pair.Request270)
	t.Logf("271Response: %s", pair.Response271)
	for _, want := range []string{
		"NM1*1P*1*SURANA*RACHNA****XX*1912143538~",
		"REF*TJ*461277465~",
		"N3*30021 ALICIA PARKWAY~",
		"N4*LAGUNA NIGUEL*CA*92677~",
		"PRV*PE*ZZ*1223G0001X~",
	} {
		if !strings.Contains(pair.Request270, want) {
			t.Fatalf("270 missing %q: %s", want, pair.Request270)
		}
	}
	for _, want := range []string{
		"ST*271*0001~",
		"EB*1**30~",
		"EB*A*IND*23***23**0~",
		"EB*A*IND*4***23**0~",
		"EB*A*IND*26***23**0~",
		"EB*A*IND*24***23**0~",
		"EB*A*IND*25***23**0.2~",
		"EB*A*IND*40***23**0.5~",
		"EB*A*IND*36***23**0.5~",
		"EB*A*IND*39***23**0.4~",
		"EB*A*IND*5***23**0~",
		"EB*A*IND*27***23**0.5~",
		"EB*A*IND*37***23**0~",
		"EB*A*IND*7***23**0.2~",
		"EB*F*IND*35***29*717.60*****Y~",
		"MSG*DENTAL CARE INDIVIDUAL CALENDAR MAXIMUM~",
		"EB*F*IND*38***29*1000.00*****Y~",
		"EB*F*IND*38***32*1000.00*****Y~",
		"EB*F*IND*23***29*703.50*****Y~",
		"EB*F*IND*24***29*703.50*****Y~",
		"EB*F*IND*28***29*703.50*****Y~",
		"EB*F*IND*4***29*703.50*****Y~",
		"EB*F*IND*41***29*703.50*****Y~",
		"EB*C*IND*35***23*50.00*****Y~",
		"EB*C*IND*35***29*0.00*****Y~",
	} {
		if !strings.Contains(pair.Response271, want) {
			t.Fatalf("271 missing %q: %s", want, pair.Response271)
		}
	}
	if strings.Contains(pair.Response271, "EB*F*FAM*35***29*0.00~") {
		t.Fatalf("271 should skip empty zero-dollar maximums: %s", pair.Response271)
	}
}

func TestBuildPairAccumulatorNetworkIndicators(t *testing.T) {
	pair := BuildPair(BuildInput{
		Appointment: models.Appointment{PatNum: "15", AptNum: "76", Ordinal: "1", FName: "A", LName: "B"},
		Report: &advanced.PatientEligibilityReport{
			Patient: advanced.PatientSnapshot{FullName: "A B", IsEligible: true, StatusLabel: "Active"},
			Plan:    advanced.PlanSnapshot{Carrier: "Carrier"},
			Maximums: []advanced.AccumulatorSummary{
				{Name: "Dental Care Individual Calendar Maximum", Kind: "maximum", Type: "calendar", Scope: "individual", Amount: 1500, Remaining: 1500},
				{Name: "Dental Care Individual Calendar Maximum (OON)", Kind: "maximum", Type: "calendar", Scope: "individual", Amount: 1000, Remaining: 1000},
				{Name: "Orthodontics Individual Lifetime Maximum", Kind: "maximum", Type: "lifetime", Scope: "individual", Amount: 1500, Remaining: 1500},
				{Name: "Orthodontics Individual Lifetime Maximum (OON)", Kind: "maximum", Type: "lifetime", Scope: "individual", Amount: 1500, Remaining: 1500},
			},
		},
		Status: "Verified",
		Now:    time.Date(2026, 5, 14, 1, 9, 0, 0, time.UTC),
	})

	for _, want := range []string{
		"EB*F*IND*35***29*1500.00*****Y~",
		"EB*F*IND*35***29*1000.00*****N~",
		"MSG*DENTAL CARE INDIVIDUAL CALENDAR MAXIMUM (OON)~",
		"EB*F*IND*38***29*1500.00*****Y~",
	} {
		if !strings.Contains(pair.Response271, want) {
			t.Fatalf("271 missing %q: %s", want, pair.Response271)
		}
	}
	if strings.Contains(pair.Response271, "ORTHODONTICS INDIVIDUAL LIFETIME MAXIMUM (OON)") {
		t.Fatalf("271 should skip duplicate OON accumulator with same amount: %s", pair.Response271)
	}
}

func TestBuildPairInactiveUsesEB6(t *testing.T) {
	pair := BuildPair(BuildInput{
		Appointment: models.Appointment{PatNum: "1", AptNum: "2", Ordinal: "1", FName: "A", LName: "B"},
		Report: &advanced.PatientEligibilityReport{
			Patient: advanced.PatientSnapshot{FullName: "A B", StatusLabel: "Inactive"},
			Plan:    advanced.PlanSnapshot{Carrier: "Carrier"},
		},
		Status: "Inactive",
		Now:    time.Date(2026, 5, 11, 12, 14, 0, 0, time.UTC),
	})

	if !strings.Contains(pair.Response271, "EB*6**30~") {
		t.Fatalf("inactive 271 missing EB*6: %s", pair.Response271)
	}
	if strings.Contains(pair.Response271, "AAA*") {
		t.Fatalf("inactive 271 should not include AAA: %s", pair.Response271)
	}
}

func TestArtifactBaseUsesInsuranceIdentityWhenNoAppointment(t *testing.T) {
	got := artifactBase(models.Appointment{
		PatNum:    "123",
		Ordinal:   "2",
		InsSubNum: "700",
		PlanNum:   "800",
	})
	want := "123_noappt_ord2_sub700_plan800_ord2"
	if got != want {
		t.Fatalf("artifactBase=%q want %q", got, want)
	}
}
