package guardian

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
	gapi "insurance-benefit-agent-go/internal/payers/guardian/api"
	gbrowser "insurance-benefit-agent-go/internal/payers/guardian/browser"
	geligibility "insurance-benefit-agent-go/internal/payers/guardian/eligibility"
	"insurance-benefit-agent-go/internal/resultwriter"
)

const PayerURL = "GuardianLife.com"

type Adapter struct{}

func NewAdapter() *Adapter { return &Adapter{} }

func (a *Adapter) PayerURL() string { return PayerURL }

func (a *Adapter) Supports(payerURL string) bool {
	switch strings.ToLower(strings.TrimSpace(payerURL)) {
	case strings.ToLower(PayerURL),
		"guardian.com",
		"guardian",
		"guardianlife",
		"guardianlife.com",
		"guardiananytime.com",
		"www.guardiananytime.com":
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
	if len(input.Appointments) == 0 {
		return summary, fmt.Errorf("[Guardian] session requires at least one appointment")
	}
	probeDir := input.ProbeOutputDir
	if probeDir == "" {
		probeDir = payers.ProbeRunDir("")
	}
	if err := os.MkdirAll(probeDir, 0o755); err != nil {
		return summary, fmt.Errorf("[Guardian] create probe dir: %w", err)
	}
	session, err := gbrowser.Launch(input)
	if err != nil {
		return summary, err
	}
	browserClosed := false
	closeBrowser := func() {
		if browserClosed {
			return
		}
		browserClosed = true
		if closeErr := session.Close(); closeErr != nil {
			log.Printf("[Guardian] browser close failed: %v", closeErr)
		}
	}
	defer closeBrowser()
	if err := session.Login(input); err != nil {
		return summary, fmt.Errorf("[Guardian] login: %w", err)
	}
	client := gapi.NewBrowserClient(session)
	log.Printf("[Guardian] phase 1 starting appointments=%d probeDir=%s", len(input.Appointments), probeDir)
	for _, appt := range input.Appointments {
		select {
		case <-ctx.Done():
			return summary, ctx.Err()
		default:
		}
		bundle, err := client.Probe(appt)
		if err != nil {
			log.Printf("[Guardian] probe failed patNum=%s aptNum=%s: %v", appt.PatNum, appt.AptNum, err)
			writeProbeError(probeDir, input.Payer.PayerURL, appt, err)
			continue
		}
		bundle.PayerURL = input.Payer.PayerURL
		bundle.Appointment = appt
		if bundle.RecordedAt == "" {
			bundle.RecordedAt = time.Now().UTC().Format(time.RFC3339)
		}
		if err := writeProbeBundle(probeDir, input.Payer.PayerURL, appt, bundle); err != nil {
			log.Printf("[Guardian] write probe failed patNum=%s aptNum=%s: %v", appt.PatNum, appt.AptNum, err)
			continue
		}
		memberCount := 0
		if bundle.Search != nil {
			for _, result := range bundle.Search.Results {
				memberCount += len(result.MemberDependent)
			}
		}
		log.Printf("[Guardian] probe written patNum=%s aptNum=%s members=%d selected=%t dentalPPO=%t",
			appt.PatNum, appt.AptNum, memberCount, bundle.SelectedMember != nil, bundle.DentalPPO != nil)
	}
	closeBrowser()
	log.Printf("[Guardian] phase 1 done; probe files in %s", probeDir)
	return summary, nil
}

func (a *Adapter) runPhase2Only(ctx context.Context, input payers.SessionInput) (payers.RunSummary, error) {
	var summary payers.RunSummary
	outputDir := filepath.Join("artifacts", "results")
	if err := os.MkdirAll(outputDir, 0o755); err != nil {
		return summary, fmt.Errorf("[Guardian] create results dir: %w", err)
	}
	appointments := input.Appointments
	if len(appointments) == 0 {
		appointments = payers.ScanProbeAppointments(input.ProbeOutputDir, input.Payer.PayerURL, "api_probe")
	}
	writer, writerErr := resultwriter.New(input.Testing, input.ScraperConfig.APIs)
	if writerErr != nil {
		log.Printf("[Guardian] resultwriter unavailable: %v", writerErr)
	}
	for _, appt := range appointments {
		select {
		case <-ctx.Done():
			return summary, ctx.Err()
		default:
		}
		report, status := a.reportForAppointment(input, outputDir, appt)
		summary.RecordAppointment(appt, status)
		log.Printf("[Guardian] result patNum=%s aptNum=%s status=%s", appt.PatNum, appt.AptNum, status)
		if writer != nil {
			writer.ApplyResult(appt, status, input.RequestedOfficeKey, nil, false)
		}
		input.QueuePDFTask(appt, status, report, outputDir)
	}
	return summary, nil
}

func (a *Adapter) reportForAppointment(input payers.SessionInput, outputDir string, appt models.Appointment) (*advanced.PatientEligibilityReport, string) {
	probePath := payers.ProbeFilePathForAppointment(input.ProbeOutputDir, input.Payer.PayerURL, appt, "api_probe")
	bundle, err := readProbeBundle(probePath)
	if err != nil {
		log.Printf("[Guardian] probe read failed patNum=%s aptNum=%s: %v", appt.PatNum, appt.AptNum, err)
		if probeErr, readErr := payers.ReadProbeErrorForAppointment(input.ProbeOutputDir, input.Payer.PayerURL, appt); readErr == nil {
			status := resultwriter.StatusForProbeErrorType(probeErr.ErrorType)
			return payers.BuildUnableToDetermineReport(appt), status
		}
		return payers.BuildUnableToDetermineReport(appt), "Error"
	}
	el := geligibility.BuildEligibilityFromProbe(bundle)
	writeDebug(outputDir, fmt.Sprintf("%s_%s_guardian_eligibility.json", appt.PatNum, appt.AptNum), el)
	if el == nil {
		return payers.BuildNotFoundReport(appt), "Not Found"
	}
	if !el.Patient.IsEligible {
		r := payers.BuildNotActiveReport(appt, el.Plan.PlanName, el.Plan.Carrier, el.Plan.GroupName)
		r.Patient.FullName = el.Patient.FullName
		r.Patient.MemberID = el.Patient.MemberID
		r.Patient.GroupNumber = el.Patient.GroupNumber
		r.Source = el.Metadata.Source
		if el.Patient.EligibilityEndDate != "" {
			r.StatusReason = fmt.Sprintf("Guardian returned coverage through %s, before the appointment date %s.", el.Patient.EligibilityEndDate, appt.AppointmentDate)
		} else {
			r.StatusReason = "Guardian returned a member record, but the coverage status is not currently active."
		}
		return r, "Inactive"
	}
	var tpCodes []string
	if appt.TreatmentPlanProcCodes != "" {
		tpCodes = strings.Split(appt.TreatmentPlanProcCodes, ",")
	}
	report := advanced.Build(el, input.OfficeCodes, tpCodes)
	if report == nil {
		report = payers.BuildUnableToDetermineReport(appt)
	}
	writeDebug(outputDir, fmt.Sprintf("%s_%s_guardian_advanced.json", appt.PatNum, appt.AptNum), report)
	return report, "Verified"
}

func writeProbeBundle(dir, payerURL string, appt models.Appointment, bundle *gapi.ProbeBundle) error {
	path := payers.ProbeFilePathForAppointment(dir, payerURL, appt, "api_probe")
	data, err := json.MarshalIndent(bundle, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

func readProbeBundle(path string) (*gapi.ProbeBundle, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var bundle gapi.ProbeBundle
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
		log.Printf("[Guardian] marshal probe error failed patNum=%s aptNum=%s: %v", appt.PatNum, appt.AptNum, marshalErr)
		return
	}
	path := payers.ProbeFilePathForAppointment(dir, payerURL, appt, "probe_error")
	if writeErr := os.WriteFile(path, data, 0o644); writeErr != nil {
		log.Printf("[Guardian] write probe error failed patNum=%s aptNum=%s: %v", appt.PatNum, appt.AptNum, writeErr)
	}
}

func writeDebug(dir, name string, value any) {
	if value == nil {
		return
	}
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return
	}
	_ = os.WriteFile(filepath.Join(dir, name), data, 0o644)
}
