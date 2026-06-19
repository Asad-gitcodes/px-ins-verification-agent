package eligibility

import (
	"testing"

	canoneligibility "insurance-benefit-agent-go/internal/eligibility"
	"insurance-benefit-agent-go/internal/models"
	dxapi "insurance-benefit-agent-go/internal/payers/dentalxchange/api"
)

func TestBuildEligibilityFromProbeUsesAetnaSplitPatientName(t *testing.T) {
	bundle := &dxapi.ProbeBundle{
		Appointment:   models.Appointment{SubFName: "SUB", SubLName: "SCRIBER"},
		SearchRequest: dxapi.SearchRequest{PatientName: "SEARCH PATIENT"},
		BenefitsPage: dxapi.PageSnapshot{HTML: `<table>
			<tr><td>Patient First Name:</td><td>ASHLEY</td></tr>
			<tr><td>Patient Last Name:</td><td>AETNA</td></tr>
		</table>`},
	}

	el := BuildEligibilityFromProbe(bundle)

	if got := el.Patient.FullName; got != "ASHLEY AETNA" {
		t.Fatalf("patient name=%q, want ASHLEY AETNA", got)
	}
}

func TestBuildEligibilityFromProbeFallsBackToSearchPatientName(t *testing.T) {
	bundle := &dxapi.ProbeBundle{
		SearchRequest: dxapi.SearchRequest{PatientName: "SEARCH PATIENT"},
	}

	el := BuildEligibilityFromProbe(bundle)

	if got := el.Patient.FullName; got != "SEARCH PATIENT" {
		t.Fatalf("patient name=%q, want SEARCH PATIENT", got)
	}
}

func TestBuildEligibilityFromProbeDoesNotUsePayerOrGroupNameAsPatient(t *testing.T) {
	bundle := &dxapi.ProbeBundle{
		Appointment: models.Appointment{FName: "JANE", LName: "DOE"},
		BenefitsPage: dxapi.PageSnapshot{HTML: `<table>
			<tr><td>Payer Name:</td><td>Aetna Dental</td></tr>
			<tr><td>Group Name:</td><td>ACME Corp</td></tr>
		</table>`},
	}

	el := BuildEligibilityFromProbe(bundle)

	if got := el.Patient.FullName; got != "JANE DOE" {
		t.Fatalf("patient name=%q, want JANE DOE", got)
	}
}

func TestBuildEligibilityFromProbeTreatsPatientTerminatedAsInactive(t *testing.T) {
	bundle := &dxapi.ProbeBundle{
		Appointment: models.Appointment{
			FName:        "Eldon Keith",
			LName:        "Roscoe",
			DOB:          "08-08-1961",
			SubscriberID: "ILA347198",
			GroupNum:     "00490913",
			CarrierName:  "Guardian",
		},
		SearchRequest: dxapi.SearchRequest{
			PayerLabel: "Guardian Life Insurance Co. of America - 64246",
		},
		BenefitsPage: dxapi.PageSnapshot{
			Text: "Information Type: Contact Following Entity for Eligibility or Benefit Information Comment: Please call 1-800-541-7846 for further assistance. (PATIENT TERMINATED)",
		},
	}

	el := BuildEligibilityFromProbe(bundle)
	if el == nil {
		t.Fatal("BuildEligibilityFromProbe returned nil")
	}
	if el.Patient.IsEligible {
		t.Fatal("IsEligible=true, want false for PATIENT TERMINATED response")
	}
	if el.Patient.MemberEligibility != "Inactive" {
		t.Fatalf("MemberEligibility=%q, want Inactive", el.Patient.MemberEligibility)
	}
}

func TestBuildTemplateCoverageCategoriesAetnaServiceLevel(t *testing.T) {
	sections := []dxBenefitSection{{
		Title: "Service Level Benefits - In Network",
		Rows: [][]string{
			{"Procedure Code", "Percentage (Pat% / Ins%) and Co-Payment ($)", "Frequency & Limitations", "Message"},
			{"D0120", "0% / 100%", "Frequency: 2 Units, for 1 Calendar Year", "DEDUCTIBLE DOES NOT APPLY"},
			{"D0431", "", "", "Not Covered"},
		},
	}, {
		Title: "Service Level Benefits - Out of Network",
		Rows: [][]string{
			{"Procedure Code", "Message"},
			{"D0120", "Not Covered"},
		},
	}}

	cats := buildTemplateCoverageCategories(sections)
	assertCoverage(t, cats, "D0120", 100)
	assertCoverage(t, cats, "D0431", 0)
}

func TestBuildTemplateCoverageCategoriesCignaMessageCodes(t *testing.T) {
	sections := []dxBenefitSection{{
		Title: "Co-Insurance",
		Rows: [][]string{
			{"Service Type", "Network", "Percentage (Pat% / Ins%)", "Message"},
			{"Restorative", "In-Network", "20% / 80%", "D2391 D2392 D2393"},
			{"Restorative", "Out-Of-Network", "30% / 70%", "D2391 D2392 D2393"},
		},
	}}

	cats := buildTemplateCoverageCategories(sections)
	assertCoverage(t, cats, "D2393", 80)
}

func TestBuildTemplateCoverageCategoriesPrincipalLimitations(t *testing.T) {
	sections := []dxBenefitSection{{
		Title: "Co-Insurance",
		Rows: [][]string{
			{"Service Type", "Participation", "Percentage"},
			{"Basic", "In Network", "80%"},
			{"Major", "In Network", "50%"},
			{"Preventative", "In Network", "100%"},
		},
	}, {
		Title: "Limitations and Maximums",
		Rows: [][]string{
			{"Service Type", "Procedure Code", "Delivery Pattern", "Participation", "Message", "Amount"},
			{"Fillings", "D2391, D2392", "", "Eligible", "Supported Teeth/Quadrants: 1, 2", ""},
			{"Implant Body", "D6010", "", "Not Eligible", "", ""},
		},
	}}

	cats := buildTemplateCoverageCategories(sections)
	assertCoverage(t, cats, "D2391", 80)
	assertCoverage(t, cats, "D6010", 0)
}

func TestBuildTemplateCoverageCategoriesDeltaMichiganPatientPercent(t *testing.T) {
	sections := []dxBenefitSection{{
		Title: "Co-Insurance",
		Rows: [][]string{
			{"Service Type", "Participation", "Network", "Percentage"},
			{"D1510", "In-Network", "Delta Dental PPO", "0%"},
			{"D0191", "In-Network", "Delta Dental PPO", "100%"},
			{"D9310", "Out-Of-Network", "", "20%"},
		},
	}}

	cats := buildTemplateCoverageCategories(sections)
	assertCoverage(t, cats, "D1510", 100)
	assertCoverage(t, cats, "D0191", 0)
	assertNoCoverage(t, cats, "D9310")
}

func TestBuildTemplateCoverageCategoriesDeltaMichiganLastVisitRows(t *testing.T) {
	sections := []dxBenefitSection{{
		Title: "Limitations and Maximums",
		Rows: [][]string{
			{"Limitations and Maximums-Benefit Level Information"},
			{"Service Type", "Coverage", "Calendar Year Amount", "Participation", "Remaining Amount", "Lifetime Amount", "Lifetime Remaining Amount", "Message"},
			{"D0120", "", "", "", "", "", "", "Last Visit: 05/09/26"},
			{"D1110", "", "", "", "", "", "", "Last Visit: 05/09/26"},
		},
	}}

	cats := buildTemplateCoverageCategories(sections)
	assertCoverageLimit(t, cats, "D0120", "Last Visit: 05/09/26")
	assertCoverageLimit(t, cats, "D1110", "Last Visit: 05/09/26")
}

func TestBuildMatrixAmeritasCoinsurance(t *testing.T) {
	rows := [][]string{
		{"Benefit Level Information"},
		{"Service Type", "Network", "Percentage (Pat% / Ins%)", "Message"},
		{"Dental Care", "Out-Of-Network", "20% / 80%", "TYPE 1 PROCEDURES COVERED AT 80% OF THE MAXIMUM ALLOWED BENEFIT. DEDUCTIBLE MAY APPLY."},
		{"Dental Care", "In-Network", "0% / 100%", "TYPE 1 PROCEDURES COVERED AT 100% OF THE MAXIMUM ALLOWABLE CHARGE. DEDUCTIBLE MAY APPLY."},
		{"Diagnostic Dental", "Out-Of-Network", "20% / 80%", ""},
		{"Diagnostic Dental", "In-Network", "0% / 100%", ""},
		{" D0140", "Out-Of-Network", "100% / 0%", ""},
		{" D0140", "In-Network", "100% / 0%", ""},
	}

	matrix := buildMatrix(rows)
	assertMatrix(t, matrix, "Dental Care", "in", "0% / 100%")
	assertMatrix(t, matrix, "Dental Care", "out", "20% / 80%")
	assertMatrix(t, matrix, "Diagnostic Dental", "in", "0% / 100%")
	assertMatrix(t, matrix, "Diagnostic Dental", "out", "20% / 80%")
}

func TestBuildMatrixDeltaMichiganPatientPercent(t *testing.T) {
	rows := [][]string{
		{"Benefit Level Information"},
		{"Service Type", "Participation", "Network", "Percentage"},
		{"Diagnostic Dental", "In-Network", "Delta Dental PPO", "0%"},
		{"Restorative", "In-Network", "Delta Dental PPO", "20%"},
		{"Dental Crowns", "In-Network", "Delta Dental PPO", "50%"},
		{"Diagnostic Dental", "Out-Of-Network", "", "0%"},
		{"Restorative", "Out-Of-Network", "", "20%"},
		{"D0120", "In-Network", "Delta Dental PPO", "0%"},
	}

	matrix := buildMatrix(rows)
	assertMatrix(t, matrix, "Diagnostic Dental", "in", "100%")
	assertMatrix(t, matrix, "Diagnostic Dental", "out", "100%")
	assertMatrix(t, matrix, "Restorative", "in", "80%")
	assertMatrix(t, matrix, "Restorative", "out", "80%")
	assertMatrix(t, matrix, "Dental Crowns", "in", "50%")
}

func TestBuildTemplateCoverageCategoriesHumanaTierColumns(t *testing.T) {
	sections := []dxBenefitSection{{
		Title: "Co-Insurance",
		Rows: [][]string{
			{"Benefit Level Information"},
			{"Service Type", "In-Network", "Message", "Out-Of-Network"},
			{"Diagnostic Dental", "0%", "Panorex-PANO XRAY D0330", "0%"},
			{"Restorative", "20%", "Fillings-FILLINGS D2140", "20%"},
			{"Dental Crowns", "50%", "Crowns-PORCELAIN D2740", "50%"},
		},
	}}

	cats := buildTemplateCoverageCategories(sections)
	assertCoverage(t, cats, "D0330", 100)
	assertCoverage(t, cats, "D2140", 80)
	assertCoverage(t, cats, "D2740", 50)
}

func TestBuildMatrixHumanaTierColumns(t *testing.T) {
	rows := [][]string{
		{"Benefit Level Information"},
		{"Service Type", "In-Network", "Message", "Out-Of-Network"},
		{"Diagnostic Dental", "0%", "Panorex-PANO XRAY D0330", "0%"},
		{"Restorative", "20%", "Fillings-FILLINGS D2140", "20%"},
	}

	matrix := buildMatrix(rows)
	assertMatrix(t, matrix, "Diagnostic Dental", "in", "0%")
	assertMatrix(t, matrix, "Diagnostic Dental", "out", "0%")
	assertMatrix(t, matrix, "Restorative", "in", "20%")
	assertMatrix(t, matrix, "Restorative", "out", "20%")
}

func TestEnrichFromBenefitsHTMLDoesNotMisclassifyCoinsuranceAsDeductible(t *testing.T) {
	html := `<div class="well"><legend>Co-Insurance</legend>
		<table><tbody>
		<tr><th colspan="4">Benefit Level Information</th></tr>
		<tr><th>Service Type</th><th>Network</th><th>Percentage (Pat% / Ins%)</th><th>Message</th></tr>
		<tr><td>Dental Care</td><td>Out-Of-Network</td><td>20% / 80%</td><td>DEDUCTIBLE MAY APPLY.</td></tr>
		<tr><td>Dental Care</td><td>In-Network</td><td>0% / 100%</td><td>DEDUCTIBLE MAY APPLY.</td></tr>
		<tr><td>Diagnostic Dental</td><td>Out-Of-Network</td><td>20% / 80%</td><td></td></tr>
		<tr><td>Diagnostic Dental</td><td>In-Network</td><td>0% / 100%</td><td></td></tr>
		</tbody></table></div>`
	el := &canoneligibility.PatientEligibility{}

	enrichFromBenefitsHTML(el, html)

	if len(el.Accumulators) != 0 {
		t.Fatalf("coinsurance table produced accumulators unexpectedly: %+v", el.Accumulators)
	}
	assertMatrix(t, el.NetworkMatrix, "Dental Care", "in", "0% / 100%")
	assertMatrix(t, el.NetworkMatrix, "Diagnostic Dental", "out", "20% / 80%")
}

func TestEnrichFromBenefitsHTMLUsesColumnsWithoutCarrierSpecificTitle(t *testing.T) {
	html := `<div class="well"><legend>Benefit Level Information</legend>
		<table><tbody>
		<tr><th>Service Type</th><th>Participation</th><th>Percentage</th><th>Message</th></tr>
		<tr><td>Preventive</td><td>In-Network</td><td>0% / 100%</td><td>D0120 D1110</td></tr>
		<tr><td>Preventive</td><td>Out-Of-Network</td><td>20% / 80%</td><td>D0120 D1110</td></tr>
		</tbody></table></div>`
	el := &canoneligibility.PatientEligibility{}

	enrichFromBenefitsHTML(el, html)

	assertMatrix(t, el.NetworkMatrix, "Preventive", "in", "0% / 100%")
	assertMatrix(t, el.NetworkMatrix, "Preventive", "out", "20% / 80%")
	assertCoverage(t, el.Coverage.Categories, "D0120", 100)
}

func TestClassifyBenefitSectionPrefersAmountTableOverServiceType(t *testing.T) {
	section := dxBenefitSection{
		Title: "Limitations and Maximums",
		Rows: [][]string{
			{"Service Type", "Participation", "Message", "Period", "Delivery Pattern", "Contract Amount", "Remaining Amount"},
			{"", "In-Network", "Plan year maximum", "Contract", "", "$2,000.00", ""},
		},
	}

	if got := classifyBenefitSection(section); got != "maximum" {
		t.Fatalf("classification=%q, want maximum", got)
	}
}

func TestClassifyBenefitSectionPrefersLimitationsTitleOverNetworkColumns(t *testing.T) {
	section := dxBenefitSection{
		Title: "Limitations and Maximums",
		Rows: [][]string{
			{"Benefit Level Information"},
			{"Service Type", "Message", "Delivery Pattern", "In- and Out- of Network", "In-Network", "Period", "Coverage", "Service Year Amount", "Out-Of-Network", "Year to Date Amount", "Remaining Amount"},
			{"Dental Care", "Seq#001", "", "", "", "Service Year", "Individual", "$2,500.00", "", "", ""},
		},
	}

	if got := classifyBenefitSection(section); got != "maximum" {
		t.Fatalf("classification=%q, want maximum", got)
	}
}

func TestClassifyBenefitSectionInfersSplitMaximumWithoutTitle(t *testing.T) {
	section := dxBenefitSection{
		Rows: [][]string{
			{"Benefit Level Information"},
			{"Service Type", "Message", "Delivery Pattern", "In- and Out- of Network", "In-Network", "Period", "Coverage", "Service Year Amount", "Out-Of-Network", "Year to Date Amount", "Remaining Amount"},
			{"Dental Care", "Seq#001", "", "", "", "Service Year", "Individual", "$2,500.00", "", "", ""},
			{"Dental Care", "", "", "", "", "Remaining", "Individual", "", "", "", "$2,388.00"},
		},
	}

	if got := classifyBenefitSection(section); got != "maximum" {
		t.Fatalf("classification=%q, want maximum", got)
	}
}

func TestDedupeAccumulatorsMergesBenefitRemainingRows(t *testing.T) {
	in := []canoneligibility.Accumulator{
		{
			AccumulatorID: "principal_maximum_calendar_individual_in_basic",
			Name:          "Basic Individual Calendar Maximum",
			Kind:          "maximum",
			Type:          "calendar",
			Scope:         "individual",
			Amount:        1500,
			Remaining:     1500,
		},
		{
			AccumulatorID: "principal_maximum_calendar_individual_in_basic",
			Name:          "Basic Individual Calendar Maximum",
			Kind:          "maximum",
			Type:          "calendar",
			Scope:         "individual",
			Amount:        165.20,
			Remaining:     165.20,
		},
	}

	got := dedupeAccumulators(in)
	if len(got) != 1 {
		t.Fatalf("deduped accumulators=%d, want 1: %+v", len(got), got)
	}
	if got[0].Amount != 1500 || got[0].Remaining != 165.20 || got[0].Used != 1334.80 {
		t.Fatalf("merged accumulator=%+v, want amount 1500 remaining 165.20 used 1334.80", got[0])
	}
}

func TestBuildMaximumsMergesHumanaSplitAmountRows(t *testing.T) {
	rows := [][]string{
		{"Benefit Level Information"},
		{"Service Type", "Message", "Delivery Pattern", "In- and Out- of Network", "In-Network", "Period", "Coverage", "Service Year Amount", "Out-Of-Network", "Year to Date Amount", "Remaining Amount"},
		{"Dental Care", "Seq#001", "", "", "", "Service Year", "Individual", "$2,500.00", "", "", ""},
		{"Dental Care", "", "", "", "", "Year to Date", "Individual", "", "", "$112.00", ""},
		{"Dental Care", "", "", "", "", "Remaining", "Individual", "", "", "", "$2,388.00"},
	}

	got := dedupeAccumulators(buildMaximums(rows))
	if len(got) != 1 {
		t.Fatalf("maximums=%d, want 1: %+v", len(got), got)
	}
	if got[0].Amount != 2500 || got[0].Remaining != 2388 || got[0].Used != 112 {
		t.Fatalf("maximum=%+v, want amount 2500 remaining 2388 used 112", got[0])
	}
}

func TestEnrichFromBenefitsHTMLAddsHumanaLimitationProvisions(t *testing.T) {
	html := `<div class="well"><legend>Limitations and Maximums</legend>
		<table><tbody>
		<tr><th>Benefit Level Information</th></tr>
		<tr><th>Service Type</th><th>Message</th><th>Delivery Pattern</th><th>In-Network</th><th>Period</th><th>Coverage</th><th>Service Year Amount</th><th>Remaining Amount</th></tr>
		<tr><td>Diagnostic Dental</td><td>Panorex X-ray</td><td>1 Visit Calendar Year</td><td></td><td></td><td></td><td></td><td></td></tr>
		<tr><td>Routine (Preventive) Dental</td><td>Sealants</td><td>1 Visit Calendar Year</td><td>Maximum Age:15</td><td></td><td></td><td></td><td></td></tr>
		<tr><td>Dental Care</td><td>Seq#001</td><td></td><td></td><td>Service Year</td><td>Individual</td><td>$2,500.00</td><td></td></tr>
		</tbody></table></div>`
	el := &canoneligibility.PatientEligibility{}

	enrichFromBenefitsHTML(el, html)

	if got := el.Plan.Provisions["Limitation: Diagnostic Dental"]; got != "Panorex X-ray 1 Visit Calendar Year" {
		t.Fatalf("diagnostic limitation=%q", got)
	}
	if got := el.Plan.Provisions["Limitation: Routine (Preventive) Dental"]; got != "Sealants 1 Visit Calendar Year Maximum Age:15" {
		t.Fatalf("preventive limitation=%q", got)
	}
	if _, ok := el.Plan.Provisions["Limitation: Dental Care"]; ok {
		t.Fatal("amount row should not become a limitation provision")
	}
}

func TestBuildMaximumsKeepsCalendarAndLifetimeColumns(t *testing.T) {
	rows := [][]string{
		{"Limitations and Maximums-Benefit Level Information"},
		{"Service Type", "Coverage", "Calendar Year Amount", "Participation", "Remaining Amount", "Lifetime Amount", "Lifetime Remaining Amount", "Message"},
		{"Dental Care", "Individual", "$1,500.00", "In-Network", "$1,360.00", "", "", ""},
		{"Orthodontics", "Individual", "", "In-Network", "", "$1,000.00", "$1,000.00", ""},
	}

	got := buildMaximums(rows)
	assertAccumulator(t, got, "maximum", "calendar", "Dental Care Individual Calendar Maximum", 1500, 1360)
	assertAccumulator(t, got, "maximum", "lifetime", "Orthodontics Individual Lifetime Maximum", 1000, 1000)
}

func TestBuildDeductiblesParsesDeltaNJHeaderNetworkColumns(t *testing.T) {
	rows := [][]string{
		{"Coverage Level Information"},
		{"Coverage - Service", "In-Network Delta Dental PPO Calendar Year Amount", "In-Network Delta Dental PPO Remaining Amount", "In-Network Delta Dental Premier Calendar Year Amount", "In-Network Delta Dental Premier Remaining Amount", "Out-Of-Network Calendar Year Amount", "Out-Of-Network Remaining Amount", " Lifetime Amount", " Lifetime Remaining Amount"},
		{"Family - Dental Care", "$150.00", "$100.00", "$150.00", "$100.00", "$150.00", "$100.00", "", ""},
		{"Individual - Dental Care", "$50.00", "$0.00", "$50.00", "$0.00", "$50.00", "$0.00", "", ""},
	}

	got := dedupeAccumulators(buildDeductibles(rows))
	assertAccumulator(t, got, "deductible", "calendar", "Dental Care Family Calendar Deductible", 150, 100)
	assertAccumulator(t, got, "deductible", "calendar", "Dental Care Individual Calendar Deductible", 50, 0)
	assertAccumulator(t, got, "deductible", "calendar", "Dental Care Family Calendar Deductible (OON)", 150, 100)
	assertAccumulator(t, got, "deductible", "calendar", "Dental Care Individual Calendar Deductible (OON)", 50, 0)
}

func TestBuildMaximumsParsesDeltaNJHeaderNetworkColumns(t *testing.T) {
	rows := [][]string{
		{"Benefit Level Information"},
		{"Service", "Coverage", "In-Network Calendar Year Amount", "In-Network Remaining Amount", "Out-Of-Network Calendar Year Amount", "Out-Of-Network Remaining Amount", "Lifetime Amount", "Lifetime Remaining Amount", "Message", "Delivery Pattern"},
		{"Dental Care", "Individual", "$2,000.00", "$717.60", "$2,000.00", "$717.60", "", "", "", ""},
	}

	got := dedupeAccumulators(buildMaximums(rows))
	assertAccumulator(t, got, "maximum", "calendar", "Dental Care Individual Calendar Maximum", 2000, 717.60)
	assertAccumulator(t, got, "maximum", "calendar", "Dental Care Individual Calendar Maximum (OON)", 2000, 717.60)
}

func assertCoverage(t *testing.T, cats []canoneligibility.CoverageCategory, code string, want int) {
	t.Helper()
	for _, cat := range cats {
		for _, svc := range cat.Services {
			if svc.Code == code {
				if svc.CoveragePercent != want {
					t.Fatalf("%s coverage=%d, want %d", code, svc.CoveragePercent, want)
				}
				return
			}
		}
	}
	t.Fatalf("%s coverage not found", code)
}

func assertCoverageLimit(t *testing.T, cats []canoneligibility.CoverageCategory, code string, want string) {
	t.Helper()
	for _, cat := range cats {
		for _, svc := range cat.Services {
			if svc.Code == code {
				if svc.Limitations != want {
					t.Fatalf("%s limitations=%q, want %q", code, svc.Limitations, want)
				}
				return
			}
		}
	}
	t.Fatalf("%s coverage not found", code)
}

func assertAccumulator(t *testing.T, accs []canoneligibility.Accumulator, kind, accType, name string, amount, remaining float64) {
	t.Helper()
	for _, acc := range accs {
		if acc.Kind == kind && acc.Type == accType && acc.Name == name {
			if acc.Amount != amount || acc.Remaining != remaining {
				t.Fatalf("%s amount/remaining=%v/%v, want %v/%v", name, acc.Amount, acc.Remaining, amount, remaining)
			}
			return
		}
	}
	t.Fatalf("accumulator %q type=%s not found in %+v", name, accType, accs)
}

func assertMatrix(t *testing.T, rows []canoneligibility.NetworkMatrixRow, category, tier, want string) {
	t.Helper()
	for _, row := range rows {
		if row.Name == category {
			if got := row.Values[tier]; got != want {
				t.Fatalf("%s[%s]=%q, want %q", category, tier, got, want)
			}
			return
		}
	}
	t.Fatalf("matrix category %q not found in %+v", category, rows)
}

func assertNoCoverage(t *testing.T, cats []canoneligibility.CoverageCategory, code string) {
	t.Helper()
	for _, cat := range cats {
		for _, svc := range cat.Services {
			if svc.Code == code {
				t.Fatalf("%s coverage found unexpectedly: %+v", code, svc)
			}
		}
	}
}
