package appointments

import (
	"strings"
	"testing"
	"time"

	"insurance-benefit-agent-go/internal/models"
)

func TestTargetDateForAddDays(t *testing.T) {
	now := time.Date(2026, time.April, 20, 15, 4, 5, 0, time.FixedZone("PDT", -7*60*60))

	if got := targetDateForAddDays(now, 1); got != "2026-04-21" {
		t.Fatalf("targetDateForAddDays(..., 1) = %q, want %q", got, "2026-04-21")
	}
	if got := targetDateForAddDays(now, 2); got != "2026-04-22" {
		t.Fatalf("targetDateForAddDays(..., 2) = %q, want %q", got, "2026-04-22")
	}
}

func TestDedupeByWorkItemIdentityKeepsDistinctAppointmentsAndOrdinals(t *testing.T) {
	rows := []models.Appointment{
		{PatNum: "123", AptNum: "1", Ordinal: "1"},
		{PatNum: "123", AptNum: "2", Ordinal: "1"},
		{PatNum: "123", AptNum: "1", Ordinal: "2"},
	}

	got := dedupeByPatNumOrdinal(rows)
	if len(got) != 3 {
		t.Fatalf("dedupeByPatNumOrdinal returned %d rows, want 3", len(got))
	}
	if got[0].AptNum != "1" || got[1].AptNum != "2" || got[2].Ordinal != "2" {
		t.Fatalf("dedupeByPatNumOrdinal kept rows %+v", got)
	}
}

func TestSelectorQueriesIncludeInsuranceRecordKeys(t *testing.T) {
	selector := NewSelector(500)
	payerQuery := selector.buildAppointmentQuery(SelectRequest{
		PayerURL:        "example.com",
		PayerIDs:        []string{"52133"},
		FutureRangeDays: 3,
	})
	dayQuery := selector.buildDayQuery(DaySelectRequest{AddDays: 3})
	patientQuery := selector.buildPatientInsuranceQuery([]PatientTarget{{PatNum: "123"}, {PatNum: "456"}})

	for name, query := range map[string]string{
		"payer":   payerQuery,
		"day":     dayQuery,
		"patient": patientQuery,
	} {
		if !strings.Contains(query, "ins.InsSubNum AS insSubNum") {
			t.Fatalf("%s query missing InsSubNum select: %s", name, query)
		}
		if !strings.Contains(query, "ins.PlanNum AS planNum") {
			t.Fatalf("%s query missing PlanNum select: %s", name, query)
		}
		if !strings.Contains(query, "ins.InsSubNum") || !strings.Contains(query, "ins.PlanNum") {
			t.Fatalf("%s query missing insurance keys in GROUP BY/select: %s", name, query)
		}
	}
}

func TestBuildPatientInsuranceQueryUsesPatientInsuranceAsPrimaryWorkItem(t *testing.T) {
	query := NewSelector(500).buildPatientInsuranceQuery([]PatientTarget{{PatNum: "16", AptNum: "120"}, {PatNum: "15"}})
	for _, want := range []string{
		"FROM (SELECT '16' AS PatNum, '120' AS AptNum UNION ALL SELECT '15' AS PatNum, '' AS AptNum) target",
		"JOIN patient p ON p.PatNum = target.PatNum",
		"JOIN patplan pp ON pp.PatNum = p.PatNum AND pp.Ordinal IN (1,2)",
		"LEFT JOIN appointment a ON target.AptNum <> '' AND a.AptNum = target.AptNum AND a.PatNum = p.PatNum",
		"ORDER BY p.PatNum ASC, pp.Ordinal ASC",
		"LIMIT 4;",
	} {
		if !strings.Contains(query, want) {
			t.Fatalf("patient insurance query missing %q:\n%s", want, query)
		}
	}
	if strings.Contains(query, "LIMIT 3;") {
		t.Fatalf("patient insurance query should not use the old hard-coded row limit:\n%s", query)
	}
}
