package dentical

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"insurance-benefit-agent-go/internal/advanced"
	"insurance-benefit-agent-go/internal/models"
	"insurance-benefit-agent-go/internal/payers"
	dapi "insurance-benefit-agent-go/internal/payers/dentical/api"
	dbrowser "insurance-benefit-agent-go/internal/payers/dentical/browser"
	deligibility "insurance-benefit-agent-go/internal/payers/dentical/eligibility"
	"insurance-benefit-agent-go/internal/resultwriter"
)

const PayerURL = "Denti-Cal.com"

type Adapter struct{}

func NewAdapter() *Adapter { return &Adapter{} }

func (a *Adapter) PayerURL() string { return PayerURL }

func (a *Adapter) Supports(payerURL string) bool {
	switch strings.ToLower(strings.TrimSpace(payerURL)) {
	case strings.ToLower(PayerURL),
		"denti-cal",
		"denti-cal.com",
		"dentical",
		"dentical.com",
		"medi-cal dental",
		"medi-caldental",
		"provider-portal.apps.prd.cammis.medi-cal.ca.gov":
		return true
	default:
		return false
	}
}

func (a *Adapter) Run(ctx context.Context, input payers.SessionInput) (payers.RunSummary, error) {
	if input.SkipProbing {
		return a.runPhase2Only(ctx, input)
	}
	return a.runPhase1(ctx, input)
}

func (a *Adapter) runPhase1(ctx context.Context, input payers.SessionInput) (payers.RunSummary, error) {
	var summary payers.RunSummary
	if !a.Supports(input.Payer.PayerURL) {
		return summary, fmt.Errorf("Denti-Cal adapter does not support payerUrl=%s", input.Payer.PayerURL)
	}
	if len(input.Appointments) == 0 {
		return summary, fmt.Errorf("Denti-Cal session requires at least one appointment")
	}

	session, err := dbrowser.Launch(input)
	if err != nil {
		return summary, err
	}
	defer func() {
		if closeErr := session.Close(); closeErr != nil {
			log.Printf("[Denti-Cal] browser close failed: %v", closeErr)
		}
	}()

	if err := session.Login(input); err != nil {
		return summary, fmt.Errorf("Denti-Cal login: %w", err)
	}

	probeDir := input.ProbeOutputDir
	if probeDir == "" {
		probeDir = payers.ProbeRunDir("")
	}
	if err := os.MkdirAll(probeDir, 0o755); err != nil {
		return summary, fmt.Errorf("create Denti-Cal probe dir: %w", err)
	}
	providerID := firstNPI(session.ProviderNPI(), input.Credential.ProviderTIN, input.Credential.ProviderName)
	if providerID != "" {
		log.Printf("[Denti-Cal] using provider NPI %s", providerID)
	}
	client := dapi.NewBrowserClient(session)

	for _, appt := range input.Appointments {
		select {
		case <-ctx.Done():
			return summary, ctx.Err()
		default:
		}
		bundle, err := client.Probe(appt, providerID)
		if err != nil {
			log.Printf("[Denti-Cal] probe failed patNum=%s aptNum=%s: %v", appt.PatNum, appt.AptNum, err)
			writeProbeError(probeDir, input.Payer.PayerURL, appt, err)
			continue
		}
		bundle.PayerURL = input.Payer.PayerURL
		if err := writeProbeBundle(probeDir, input.Payer.PayerURL, appt, bundle); err != nil {
			log.Printf("[Denti-Cal] write probe failed patNum=%s aptNum=%s: %v", appt.PatNum, appt.AptNum, err)
		}
		status := statusFromBundle(bundle)
		log.Printf("[Denti-Cal] probe written patNum=%s aptNum=%s status=%s evc=%s message=%q",
			appt.PatNum, appt.AptNum, status, evc(bundle), message(bundle))
	}
	return summary, nil
}

func (a *Adapter) runPhase2Only(ctx context.Context, input payers.SessionInput) (payers.RunSummary, error) {
	var summary payers.RunSummary
	if !a.Supports(input.Payer.PayerURL) {
		return summary, fmt.Errorf("Denti-Cal adapter does not support payerUrl=%s", input.Payer.PayerURL)
	}
	outputDir := filepath.Join("artifacts", "results")
	if err := os.MkdirAll(outputDir, 0o755); err != nil {
		return summary, fmt.Errorf("create Denti-Cal results dir: %w", err)
	}
	appointments := input.Appointments
	if len(appointments) == 0 {
		appointments = payers.ScanProbeAppointments(input.ProbeOutputDir, input.Payer.PayerURL, "api_probe")
	}
	writer, writerErr := resultwriter.New(input.Testing, input.ScraperConfig.APIs)
	if writerErr != nil {
		log.Printf("[Denti-Cal] resultwriter unavailable: %v", writerErr)
	}
	for _, appt := range appointments {
		select {
		case <-ctx.Done():
			return summary, ctx.Err()
		default:
		}
		bundle, err := readProbeBundle(payers.ProbeFilePathForAppointment(input.ProbeOutputDir, input.Payer.PayerURL, appt, "api_probe"))
		if err != nil {
			status := "Error"
			report := payers.BuildUnableToDetermineReport(appt)
			if probeErr, readErr := payers.ReadProbeErrorForAppointment(input.ProbeOutputDir, input.Payer.PayerURL, appt); readErr == nil {
				status = resultwriter.StatusForProbeErrorType(probeErr.ErrorType)
			}
			summary.RecordAppointment(appt, status)
			log.Printf("[Denti-Cal] probe read failed patNum=%s aptNum=%s status=%s: %v", appt.PatNum, appt.AptNum, status, err)
			if writer != nil {
				writer.ApplyResult(appt, status, input.RequestedOfficeKey, nil, false)
			}
			input.QueuePDFTask(appt, status, report, outputDir)
			continue
		}
		status := statusFromBundle(bundle)
		report := reportFromBundle(appt, bundle)
		summary.RecordAppointment(appt, status)
		log.Printf("[Denti-Cal] result patNum=%s aptNum=%s status=%s evc=%s message=%q",
			appt.PatNum, appt.AptNum, status, evc(bundle), message(bundle))
		if writer != nil {
			writer.ApplyResult(appt, status, input.RequestedOfficeKey, nil, false)
		}
		input.QueuePDFTask(appt, status, report, outputDir)
	}
	return summary, nil
}

func statusFromBundle(bundle *dapi.ProbeBundle) string {
	if bundle == nil || bundle.Response == nil || bundle.Response.Results == nil {
		return "Not Found"
	}
	if strings.EqualFold(strings.TrimSpace(bundle.Response.Results.FoundElig), "Y") {
		return "Verified"
	}
	return "Inactive"
}

func reportFromBundle(appt models.Appointment, bundle *dapi.ProbeBundle) *advanced.PatientEligibilityReport {
	el := deligibility.BuildEligibilityFromProbe(bundle)
	if el == nil {
		return payers.BuildNotFoundReport(appt)
	}
	// Denti-Cal exposes eligibility and message/provision data, but not CDT-level
	// benefits. Build a basic report with no procedure-code rows.
	report := advanced.Build(el, nil, nil)
	if report == nil {
		return payers.BuildUnableToDetermineReport(appt)
	}
	return report
}

func writeProbeBundle(dir, payerURL string, appt models.Appointment, bundle *dapi.ProbeBundle) error {
	path := payers.ProbeFilePathForAppointment(dir, payerURL, appt, "api_probe")
	data, err := json.MarshalIndent(bundle, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

func readProbeBundle(path string) (*dapi.ProbeBundle, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var bundle dapi.ProbeBundle
	if err := json.Unmarshal(data, &bundle); err != nil {
		return nil, err
	}
	return &bundle, nil
}

func writeProbeError(dir, payerURL string, appt models.Appointment, err error) {
	artifact := payers.ProbeErrorArtifact{
		Error:      err.Error(),
		ErrorType:  payers.ClassifyProbeError(err),
		RecordedAt: time.Now().UTC().Format(time.RFC3339),
	}
	data, marshalErr := json.MarshalIndent(artifact, "", "  ")
	if marshalErr != nil {
		log.Printf("[Denti-Cal] marshal probe error failed patNum=%s aptNum=%s: %v", appt.PatNum, appt.AptNum, marshalErr)
		return
	}
	path := payers.ProbeFilePathForAppointment(dir, payerURL, appt, "probe_error")
	if writeErr := os.WriteFile(path, data, 0o644); writeErr != nil {
		log.Printf("[Denti-Cal] write probe error failed patNum=%s aptNum=%s: %v", appt.PatNum, appt.AptNum, writeErr)
	}
}

func evc(bundle *dapi.ProbeBundle) string {
	if bundle == nil || bundle.Response == nil || bundle.Response.Results == nil {
		return ""
	}
	return bundle.Response.Results.EVCTraceNumber
}

func message(bundle *dapi.ProbeBundle) string {
	if bundle == nil || bundle.Response == nil || bundle.Response.Results == nil {
		return ""
	}
	return bundle.Response.Results.TextMessage
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" {
			return value
		}
	}
	return ""
}

func firstNPI(values ...string) string {
	for _, value := range values {
		value = strings.TrimSpace(value)
		if len(value) == 10 && allDigits(value) {
			return value
		}
	}
	return ""
}

func allDigits(value string) bool {
	for _, r := range value {
		if r < '0' || r > '9' {
			return false
		}
	}
	return value != ""
}
