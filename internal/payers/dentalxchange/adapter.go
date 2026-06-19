package dentalxchange

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
	dxapi "insurance-benefit-agent-go/internal/payers/dentalxchange/api"
	dxbrowser "insurance-benefit-agent-go/internal/payers/dentalxchange/browser"
	dxeligibility "insurance-benefit-agent-go/internal/payers/dentalxchange/eligibility"
	"insurance-benefit-agent-go/internal/resultwriter"
)

const PayerURL = "DentalXChange.com"

type Adapter struct{}

func NewAdapter() *Adapter { return &Adapter{} }

func (a *Adapter) PayerURL() string { return PayerURL }

func (a *Adapter) Supports(payerURL string) bool {
	switch strings.ToLower(strings.TrimSpace(payerURL)) {
	case strings.ToLower(PayerURL), "claimconnect.dentalxchange.com", "dentalxchange", "claimconnect":
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
		return summary, fmt.Errorf("[DentalXChange] session requires at least one appointment")
	}
	probeDir := input.ProbeOutputDir
	if probeDir == "" {
		probeDir = payers.ProbeRunDir("")
	}
	if err := os.MkdirAll(probeDir, 0o755); err != nil {
		return summary, fmt.Errorf("[DentalXChange] create probe dir: %w", err)
	}

	session, err := dxbrowser.Launch(input)
	if err != nil {
		return summary, err
	}
	closed := false
	closeBrowser := func() {
		if closed {
			return
		}
		closed = true
		if closeErr := session.Close(); closeErr != nil {
			log.Printf("[DentalXChange] browser close failed: %v", closeErr)
		}
	}
	defer closeBrowser()

	if err := session.Login(input); err != nil {
		return summary, fmt.Errorf("[DentalXChange] login failed: %w", err)
	}
	cookies, err := session.Cookies()
	if err != nil {
		return summary, fmt.Errorf("[DentalXChange] get browser cookies: %w", err)
	}
	client := dxapi.NewClient(cookies).
		WithProvider(input.Credential.ProviderName, input.Credential.ProviderTIN)

	log.Printf("[DentalXChange] phase 1 starting appointments=%d probeDir=%s", len(input.Appointments), probeDir)
	for _, appt := range input.Appointments {
		select {
		case <-ctx.Done():
			return summary, ctx.Err()
		default:
		}
		if err := session.OpenEligibilitySearch(); err != nil {
			log.Printf("[DentalXChange] probe failed patNum=%s aptNum=%s payer=%q: %v",
				appt.PatNum, appt.AptNum, appt.CarrierName, err)
			writeProbeError(probeDir, input.Payer.PayerURL, appt, err)
			continue
		}
		search, err := client.WithStartURL(session.CurrentURL()).PrepareSearch(ctx, appt)
		if err == nil {
			log.Printf("[DentalXChange] search prepared patNum=%s aptNum=%s provider=%s payer=%s",
				appt.PatNum, appt.AptNum, search.BillingProvider, search.PayerValue)
		}
		if err != nil {
			log.Printf("[DentalXChange] probe failed patNum=%s aptNum=%s payer=%q: %v",
				appt.PatNum, appt.AptNum, appt.CarrierName, err)
			writeProbeError(probeDir, input.Payer.PayerURL, appt, err)
			continue
		}
		eligibilityPage, benefitsPage, err := session.SubmitEligibility(ctx, appt, search)
		if err != nil && strings.Contains(err.Error(), "Provider Identification") {
			log.Printf("[DentalXChange] provider rejected patNum=%s aptNum=%s payer=%q, retrying with first provider: %v",
				appt.PatNum, appt.AptNum, appt.CarrierName, err)
			openErr := session.OpenEligibilitySearch()
			if openErr == nil {
				if retrySearch, retryErr := client.WithStartURL(session.CurrentURL()).ForceFirstProvider().PrepareSearch(ctx, appt); retryErr == nil {
					eligibilityPage, benefitsPage, err = session.SubmitEligibility(ctx, appt, retrySearch)
				}
			}
		}
		if err != nil {
			log.Printf("[DentalXChange] probe failed patNum=%s aptNum=%s payer=%q: %v",
				appt.PatNum, appt.AptNum, appt.CarrierName, err)
			writeProbeError(probeDir, input.Payer.PayerURL, appt, err)
			continue
		}
		bundle := &dxapi.ProbeBundle{
			Appointment:     appt,
			RecordedAt:      time.Now().UTC().Format(time.RFC3339),
			SearchRequest:   search,
			EligibilityPage: eligibilityPage,
			BenefitsPage:    benefitsPage,
		}
		bundle.PayerURL = input.Payer.PayerURL
		if err := writeProbeBundle(probeDir, appt, input.Payer.PayerURL, bundle); err != nil {
			log.Printf("[DentalXChange] probe write failed patNum=%s aptNum=%s: %v", appt.PatNum, appt.AptNum, err)
			continue
		}
		log.Printf("[DentalXChange] probe summary patNum=%s aptNum=%s payer=%q eligibilityBytes=%d benefitsBytes=%d",
			appt.PatNum, appt.AptNum, bundle.SearchRequest.PayerLabel, bundle.EligibilityPage.Bytes, bundle.BenefitsPage.Bytes)
	}
	closeBrowser()
	log.Printf("[DentalXChange] phase 1 done; raw probe files kept in %s", probeDir)
	return summary, nil
}

func (a *Adapter) runPhase2Only(ctx context.Context, input payers.SessionInput) (payers.RunSummary, error) {
	var summary payers.RunSummary
	outputDir := filepath.Join("artifacts", "results")
	if err := os.MkdirAll(outputDir, 0o755); err != nil {
		return summary, fmt.Errorf("[DentalXChange] create results dir: %w", err)
	}
	appointments := input.Appointments
	if len(appointments) == 0 {
		appointments = payers.ScanProbeAppointments(input.ProbeOutputDir, input.Payer.PayerURL, "api_probe")
		log.Printf("[DentalXChange] probe scan found %d appointments in %s", len(appointments), input.ProbeOutputDir)
	}
	if len(appointments) == 0 {
		log.Printf("[DentalXChange] no probe files found, nothing to postprocess")
		return summary, nil
	}

	writer, writerErr := resultwriter.New(input.Testing, input.ScraperConfig.APIs)
	if writerErr != nil {
		log.Printf("[DentalXChange] resultwriter unavailable: %v", writerErr)
	}

	for _, appt := range appointments {
		select {
		case <-ctx.Done():
			return summary, ctx.Err()
		default:
		}
		var tpCodes []string
		if appt.TreatmentPlanProcCodes != "" {
			tpCodes = strings.Split(appt.TreatmentPlanProcCodes, ",")
		}
		probePath := payers.ProbeFilePathForAppointment(input.ProbeOutputDir, input.Payer.PayerURL, appt, "api_probe")
		bundle, readErr := readProbeBundle(probePath)

		var report *advanced.PatientEligibilityReport
		statusOverride := ""
		if readErr != nil {
			log.Printf("[DentalXChange] probe read failed patNum=%s aptNum=%s: %v", appt.PatNum, appt.AptNum, readErr)
			report = payers.BuildUnableToDetermineReport(appt)
			if probeErr, err := payers.ReadProbeErrorForAppointment(input.ProbeOutputDir, input.Payer.PayerURL, appt); err == nil {
				statusOverride = resultwriter.StatusForProbeErrorType(probeErr.ErrorType)
				log.Printf("[DentalXChange] probe error result patNum=%s aptNum=%s errorType=%s error=%q", appt.PatNum, appt.AptNum, probeErr.ErrorType, probeErr.Error)
				if reason := payers.ProbeErrorStatusReason(probeErr); reason != "" {
					report.StatusReason = reason
				}
			}
		} else {
			el := dxeligibility.BuildEligibilityFromProbe(bundle)
			if el == nil {
				report = payers.BuildUnableToDetermineReport(appt)
			} else if !el.Patient.IsEligible {
				r := payers.BuildNotActiveReport(appt, el.Plan.PlanName, el.Plan.Carrier, el.Plan.GroupName)
				r.Patient.FullName = el.Patient.FullName
				r.Patient.MemberID = el.Patient.MemberID
				r.Patient.GroupNumber = el.Patient.GroupNumber
				r.Source = el.Metadata.Source
				r.StatusReason = dentalXChangeInactiveReason(bundle)
				report = r
			} else {
				report = advanced.Build(el, input.OfficeCodes, tpCodes)
				if report == nil {
					report = payers.BuildUnableToDetermineReport(appt)
				}
			}
			writeResultsFile(outputDir, fmt.Sprintf("%s_%s_eligibility.json", appt.PatNum, appt.AptNum), el)
			writeResultsFile(outputDir, fmt.Sprintf("%s_%s_advanced.json", appt.PatNum, appt.AptNum), report)
		}

		status := apptStatus(report)
		if statusOverride != "" {
			status = statusOverride
		}
		summary.RecordAppointment(appt, status)
		log.Printf("[DentalXChange] result patNum=%s aptNum=%s status=%s", appt.PatNum, appt.AptNum, status)
		if writer != nil {
			writer.ApplyResult(appt, status, input.RequestedOfficeKey, nil, false)
		}
		input.QueuePDFTask(appt, status, report, outputDir)
	}
	return summary, nil
}

func dentalXChangeInactiveReason(bundle *dxapi.ProbeBundle) string {
	if bundle == nil {
		return "DentalXChange ClaimConnect indicated the member is not currently active."
	}
	text := strings.ToLower(bundle.EligibilityPage.Text + " " + bundle.BenefitsPage.Text)
	switch {
	case strings.Contains(text, "patient terminated"):
		return "DentalXChange ClaimConnect reported: PATIENT TERMINATED."
	case strings.Contains(text, "subscriber terminated"):
		return "DentalXChange ClaimConnect reported: SUBSCRIBER TERMINATED."
	case strings.Contains(text, "member terminated"):
		return "DentalXChange ClaimConnect reported: MEMBER TERMINATED."
	case strings.Contains(text, "coverage terminated"):
		return "DentalXChange ClaimConnect reported terminated coverage."
	default:
		return "DentalXChange ClaimConnect indicated the member is not currently active."
	}
}

func writeProbeBundle(dir string, appt models.Appointment, payerURL string, bundle *dxapi.ProbeBundle) error {
	path := payers.ProbeFilePathForAppointment(dir, payerURL, appt, "api_probe")
	data, err := json.MarshalIndent(bundle, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal probe: %w", err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("write probe: %w", err)
	}
	return nil
}

func readProbeBundle(path string) (*dxapi.ProbeBundle, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read probe: %w", err)
	}
	var bundle dxapi.ProbeBundle
	if err := json.Unmarshal(data, &bundle); err != nil {
		return nil, fmt.Errorf("unmarshal probe: %w", err)
	}
	return &bundle, nil
}

func writeProbeError(dir string, payerURL string, appt models.Appointment, probeErr error) {
	path := payers.ProbeFilePathForAppointment(dir, payerURL, appt, "probe_error")
	payload := map[string]any{
		"recordedAt":  time.Now().UTC().Format(time.RFC3339),
		"appointment": appt,
		"error":       probeErr.Error(),
		"errorType":   payers.ClassifyProbeError(probeErr),
	}
	data, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return
	}
	_ = os.WriteFile(path, data, 0o644)
}

func writeResultsFile(dir, filename string, payload any) {
	data, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		log.Printf("[DentalXChange] marshal results %s: %v", filename, err)
		return
	}
	if err := os.WriteFile(filepath.Join(dir, filename), data, 0o644); err != nil {
		log.Printf("[DentalXChange] write results %s: %v", filename, err)
	}
}

func apptStatus(report *advanced.PatientEligibilityReport) string {
	if report == nil {
		return resultwriter.ApptStatusError
	}
	switch report.Patient.StatusLabel {
	case "Not Found":
		return resultwriter.ApptStatusNotFound
	case "Unable to Determine":
		return resultwriter.ApptStatusError
	default:
		return resultwriter.EligibilityStatus(report.Patient.IsEligible)
	}
}
