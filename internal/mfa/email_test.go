package mfa

import "testing"

func TestExtractSixDigitCodePrefersDentaQuestCodeText(t *testing.T) {
	body := `Hi patientxpress,
Here is the verification email you requested.
Can't use the link? Enter a code instead: 125772
You can contact us at 866-556-2388 with any concerns.`

	got := extractSixDigitCode(body)
	if got != "125772" {
		t.Fatalf("extractSixDigitCode() = %q, want %q", got, "125772")
	}
}
