package odetrans

import (
	"strings"
	"testing"

	"insurance-benefit-agent-go/internal/models"
)

func TestBuildInsertSQLUsesActualAppointmentValuesAndLinksResponse(t *testing.T) {
	sql := BuildInsertSQL(models.Appointment{
		CarrierNum: "230",
		PatNum:     "6812",
		PlanNum:    "3047",
		InsSubNum:  "2979",
	}, Pair{
		Request270:  "ST*270*0001~",
		Response271: "ST*271*0001~",
	})

	for _, want := range []string{
		"START TRANSACTION;",
		"VALUES ('ST*270*0001~');",
		"NOW(), 20, 24, 0, 0,",
		"0, 0, 230, 0, 6812,",
		"3047, 2979, '', '', '', 0",
		"VALUES ('ST*271*0001~');",
		"NOW(), 20, 25, 0, 0,",
		"SET AckEtransNum = @etrans271,",
		"Note = 'Normal 271 response.'",
		"COMMIT;",
	} {
		if !strings.Contains(sql, want) {
			t.Fatalf("SQL missing %q:\n%s", want, sql)
		}
	}
}

func TestSQLQuoteEscapesSingleQuotes(t *testing.T) {
	if got := sqlQuote("A'B"); got != "'A''B'" {
		t.Fatalf("sqlQuote=%q", got)
	}
}
