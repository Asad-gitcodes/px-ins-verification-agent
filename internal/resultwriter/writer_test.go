package resultwriter

import "testing"

func TestAppointmentFieldValue(t *testing.T) {
	tests := map[string]string{
		ApptStatusVerified:           "V1",
		ApptStatusInactive:           "NV1: Coverage found but inactive",
		ApptStatusNotFound:           "NV1: Patient/member not found",
		ApptStatusError:              "NV1: Invalid/missing member info",
		ApptStatusPayerSystemFailure: "NV1: Payer/system failure",
		"":                           "NV1: Invalid/missing member info",
	}

	for status, want := range tests {
		if got := AppointmentFieldValue(status); got != want {
			t.Fatalf("AppointmentFieldValue(%q) = %q, want %q", status, got, want)
		}
	}
}

func TestAppointmentOrdinalFieldValueSecondary(t *testing.T) {
	tests := map[string]string{
		ApptStatusVerified: "V2",
		ApptStatusInactive: "NV2: Coverage found but inactive",
		ApptStatusNotFound: "NV2: Patient/member not found",
	}

	for status, want := range tests {
		if got := AppointmentOrdinalFieldValue(status, "2"); got != want {
			t.Fatalf("AppointmentOrdinalFieldValue(%q, 2) = %q, want %q", status, got, want)
		}
	}
}

func TestStatusForProbeErrorType(t *testing.T) {
	tests := map[string]string{
		"patient_error": ApptStatusError,
		"unknown_error": ApptStatusError,
		"":              ApptStatusError,
		"payer_error":   ApptStatusPayerSystemFailure,
		"system_error":  ApptStatusPayerSystemFailure,
	}

	for errorType, want := range tests {
		if got := StatusForProbeErrorType(errorType); got != want {
			t.Fatalf("StatusForProbeErrorType(%q) = %q, want %q", errorType, got, want)
		}
	}
}

func TestBuildPDFFileNameUsesPXV2OrdinalAndStatus(t *testing.T) {
	tests := []struct {
		status  string
		ordinal string
		want    string
	}{
		{ApptStatusVerified, "1", "PXV2_Primary_Electronic_Benefits_Active.pdf"},
		{ApptStatusVerified, "2", "PXV2_Secondary_Electronic_Benefits_Active.pdf"},
		{ApptStatusInactive, "1", "PXV2_Primary_Electronic_Benefits_NotActive.pdf"},
		{ApptStatusNotFound, "2", "PXV2_Secondary_Electronic_Benefits_NotActive.pdf"},
	}
	for _, tt := range tests {
		if got := buildPDFFileName(tt.status, tt.ordinal); got != tt.want {
			t.Fatalf("buildPDFFileName(%q,%q)=%q want %q", tt.status, tt.ordinal, got, tt.want)
		}
	}
}
