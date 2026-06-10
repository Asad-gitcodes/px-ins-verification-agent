package payers

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"insurance-benefit-agent-go/internal/models"
)

const (
	ProbeErrorPatient     = "patient_error"
	ProbeErrorPayer       = "payer_error"
	ProbeErrorSystem      = "system_error"
	ProbeErrorUnsupported = "unsupported_payer"
	ProbeErrorUnknown     = "unknown_error"
)

func ClassifyProbeError(err error) string {
	if err == nil {
		return ProbeErrorUnknown
	}
	text := strings.ToLower(err.Error())
	patientSignals := []string{
		"invalid/missing subscriber",
		"invalid subscriber",
		"missing subscriber",
		"subscriber/insured id",
		"member not found",
		"patient not found",
		"no matching patient",
		"no matching member",
		"dob mismatch",
		"name mismatch",
	}
	for _, signal := range patientSignals {
		if strings.Contains(text, signal) {
			return ProbeErrorPatient
		}
	}

	unsupportedSignals := []string{
		"payer lookup",
		"no payer matched",
		"unsupported payer",
		"unsupported_payer",
	}
	for _, signal := range unsupportedSignals {
		if strings.Contains(text, signal) {
			return ProbeErrorUnsupported
		}
	}

	payerSignals := []string{
		"site unavailable",
		"portal unavailable",
		"payer unavailable",
		"gateway timeout",
		"service unavailable",
		"too many requests",
		"status code 500",
		"status code 502",
		"status code 503",
		"status code 504",
		"http 429",
		"http 500",
		"http 502",
		"http 503",
		"http 504",
	}
	for _, signal := range payerSignals {
		if strings.Contains(text, signal) {
			return ProbeErrorPayer
		}
	}

	systemSignals := []string{
		"browser launch",
		"login failed",
		"mfa",
		"context deadline exceeded",
		"i/o timeout",
		"connection refused",
	}
	for _, signal := range systemSignals {
		if strings.Contains(text, signal) {
			return ProbeErrorSystem
		}
	}

	return ProbeErrorUnknown
}

func ProbeErrorStatusReason(artifact *ProbeErrorArtifact) string {
	if artifact == nil {
		return ""
	}
	errText := strings.TrimSpace(artifact.Error)
	if errText == "" {
		return ""
	}
	if artifact.ErrorType == ProbeErrorUnsupported || strings.Contains(strings.ToLower(errText), "no payer matched") {
		return fmt.Sprintf("Payer not supported: %s.", strings.TrimRight(errText, "."))
	}
	return fmt.Sprintf("Probe failed: %s.", strings.TrimRight(errText, "."))
}

type ProbeErrorArtifact struct {
	Error      string `json:"error"`
	ErrorType  string `json:"errorType"`
	RecordedAt string `json:"recordedAt,omitempty"`
}

func ReadProbeError(dir, payerURL, patNum, aptNum string) (*ProbeErrorArtifact, error) {
	path := ProbeFilePath(dir, payerURL, patNum, aptNum, "probe_error")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var artifact ProbeErrorArtifact
	if err := json.Unmarshal(data, &artifact); err != nil {
		return nil, fmt.Errorf("unmarshal probe error: %w", err)
	}
	if strings.TrimSpace(artifact.ErrorType) == "" {
		artifact.ErrorType = ProbeErrorUnknown
	}
	return &artifact, nil
}

func ReadProbeErrorForAppointment(dir, payerURL string, appointment models.Appointment) (*ProbeErrorArtifact, error) {
	return ReadProbeError(dir, payerURL, appointment.PatNum, ProbeAppointmentSegment(appointment))
}
