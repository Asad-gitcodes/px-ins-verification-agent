package payers

import (
	"errors"
	"testing"
)

func TestClassifyProbeError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want string
	}{
		{
			name: "invalid subscriber is patient error",
			err:  errors.New("payer rejected: Invalid/Missing Subscriber/Insured ID."),
			want: ProbeErrorPatient,
		},
		{
			name: "member not found is patient error",
			err:  errors.New("member not found for submitted patient"),
			want: ProbeErrorPatient,
		},
		{
			name: "http 503 is payer error",
			err:  errors.New("eligibility request returned HTTP 503"),
			want: ProbeErrorPayer,
		},
		{
			name: "payer lookup miss is unsupported",
			err:  errors.New(`payer lookup: no payer matched payerID="35182" carrier="CoreSource " options=572`),
			want: ProbeErrorUnsupported,
		},
		{
			name: "login failure is system error",
			err:  errors.New("login failed: MFA code expired"),
			want: ProbeErrorSystem,
		},
		{
			name: "unmatched text is unknown",
			err:  errors.New("unexpected response shape"),
			want: ProbeErrorUnknown,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ClassifyProbeError(tt.err); got != tt.want {
				t.Fatalf("ClassifyProbeError() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestProbeErrorStatusReason(t *testing.T) {
	reason := ProbeErrorStatusReason(&ProbeErrorArtifact{
		Error:     `payer lookup: no payer matched payerID="35182" carrier="CoreSource " options=572`,
		ErrorType: ProbeErrorUnsupported,
	})

	want := `Payer not supported: payer lookup: no payer matched payerID="35182" carrier="CoreSource " options=572.`
	if reason != want {
		t.Fatalf("reason=%q, want %q", reason, want)
	}
}
