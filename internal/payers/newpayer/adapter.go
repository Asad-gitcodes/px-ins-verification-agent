// Package newpayer is a scaffolding template for onboarding a new insurance payer.
//
// SETUP CHECKLIST:
//   1. Copy this whole folder: cp -r internal/payers/newpayer internal/payers/YOURPAYER
//   2. Rename every occurrence of "newpayer" / "NewPayer" / "NEWPAYER" to your payer name.
//   3. Set PayerURL to the canonical portal domain (matches what PatCon sends in payer.PayerURL).
//   4. Fill in every TODO(newpayer) comment in this file and in browser/session.go.
//   5. Register the adapter in internal/app/app.go (see bottom of that file for examples).
//   6. Run: go build ./... to confirm it compiles.

package newpayer

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
	newpayerbrowser "insurance-benefit-agent-go/internal/payers/newpayer/browser"
	newpayereligibility "insurance-benefit-agent-go/internal/payers/newpayer/eligibility"
	"insurance-benefit-agent-go/internal/resultwriter"
)

// TODO(newpayer): Set this to the canonical payerUrl string sent by the PatCon API.
// Examples from existing payers: "DentaQuest.com", "metlife.com", "DeltaDentalIns.com"
const PayerURL = "newpayer.example.com"

type Adapter struct{}

func NewAdapter() *Adapter { return &Adapter{} }

func (a *Adapter) PayerURL() string { return PayerURL }

func (a *Adapter) Supports(payerURL string) bool {
	return strings.EqualFold(payerURL, PayerURL)
}

// Run is the top-level payer pipeline. It orchestrates two phases:
//   - Phase 1 (browser open): log in, probe each patient, write raw probe JSON to disk.
//   - Phase 2 (browser closed): read probes, build eligibility reports, write PDFs.
func (a *Adapter) Run(ctx context.Context, input payers.SessionInput) (payers.RunSummary, error) {
	var summary payers.RunSummary

	if !a.Supports(input.Payer.PayerURL) {
		return summary, fmt.Errorf("NewPayer adapter does not support payerUrl=%s", input.Payer.PayerURL)
	}

	if input.SkipProbing {
		return a.runPhase2Only(ctx, input)
	}

	if len(input.Appointments) == 0 {
		return summary, fmt.Errorf("NewPayer session requires at least one appointment")
	}

	runStamp := time.Now().UTC().Format("2006-01-02T15-04-05Z")
	outputDir := filepath.Join("artifacts", sanitize(input.RequestedOfficeKey), runStamp, sanitize(PayerURL))
	probeDir := input.ProbeOutputDir
	if probeDir == "" {
		probeDir = filepath.Join(outputDir, "_tmp_probe")
	}
	if err := os.MkdirAll(probeDir, 0o755); err != nil {
		return summary, fmt.Errorf("NewPayer create probe dir: %w", err)
	}

	// ── Phase 1: open browser, login, probe every patient ────────────────────
	session, err := newpayerbrowser.Launch(input)
	if err != nil {
		return summary, fmt.Errorf("NewPayer browser launch: %w", err)
	}
	browserClosed := false
	closeBrowser := func() {
		if browserClosed {
			return
		}
		browserClosed = true
		if cerr := session.Close(); cerr != nil {
			log.Printf("[NewPayer] browser close failed: %v", cerr)
		}
	}
	defer closeBrowser()

	if err := session.Login(input); err != nil {
		return summary, fmt.Errorf("NewPayer login: %w", err)
	}
	log.Printf("[NewPayer] login complete")

	type task struct {
		appointment models.Appointment
		tpCodes     []string
		probePath   string                          // set on probe success
		report      *advanced.PatientEligibilityReport // set on probe failure (skip phase 2)
	}
	var tasks []task

	for _, appt := range input.Appointments {
		select {
		case <-ctx.Done():
			closeBrowser()
			return summary, ctx.Err()
		default:
		}

		var tpCodes []string
		if appt.TreatmentPlanProcCodes != "" {
			tpCodes = strings.Split(appt.TreatmentPlanProcCodes, ",")
		}

		t := task{appointment: appt, tpCodes: tpCodes}

		// TODO(newpayer): Replace ProbePatient with your actual scrape call.
		// It should navigate to the member search page, enter patient details,
		// and return the raw data (as *RawProbeData) needed to build eligibility.
		rawAny, probeErr := session.ProbePatient(appt)
		if probeErr != nil {
			log.Printf("[NewPayer] probe failed patNum=%s aptNum=%s: %v", appt.PatNum, appt.AptNum, probeErr)
			writeProbeError(probeDir, appt, probeErr)
			t.report = payers.BuildNotFoundReport(appt)
		} else if rawAny == nil {
			t.report = payers.BuildUnableToDetermineReport(appt)
		} else {
			raw, ok := rawAny.(*RawProbeData)
			if !ok {
				t.report = payers.BuildUnableToDetermineReport(appt)
			} else {
				probePath, werr := writeProbeResult(probeDir, appt, raw)
				if werr != nil {
					log.Printf("[NewPayer] probe write failed patNum=%s: %v", appt.PatNum, werr)
				} else {
					t.probePath = probePath
					log.Printf("[NewPayer] probe written patNum=%s aptNum=%s file=%s", appt.PatNum, appt.AptNum, probePath)
				}
			}
		}
		tasks = append(tasks, t)
	}

	closeBrowser()
	log.Printf("[NewPayer] phase 1 done; %d probe files in %s", len(tasks), probeDir)

	// ── Phase 2: build reports from probe files ───────────────────────────────
	writer, writerErr := resultwriter.New(input.Testing, input.ScraperConfig.APIs)
	if writerErr != nil {
		log.Printf("[NewPayer] resultwriter unavailable: %v", writerErr)
	}

	for _, t := range tasks {
		select {
		case <-ctx.Done():
			return summary, ctx.Err()
		default:
		}

		report := t.report
		if report == nil && t.probePath != "" {
			raw, rerr := readProbeResult(t.probePath)
			if rerr != nil {
				log.Printf("[NewPayer] probe read failed patNum=%s: %v", t.appointment.PatNum, rerr)
				report = payers.BuildUnableToDetermineReport(t.appointment)
			} else {
				el := newpayereligibility.Build(raw)
				if el == nil {
					report = payers.BuildUnableToDetermineReport(t.appointment)
				} else if !el.Patient.IsEligible {
					r := payers.BuildNotActiveReport(t.appointment, el.Plan.PlanName, el.Plan.Carrier, el.Plan.GroupName)
					r.Patient.MemberID = el.Patient.MemberID
					r.Patient.FullName = el.Patient.FullName
					report = r
				} else {
					report = advanced.Build(el, input.OfficeCodes, t.tpCodes)
					if report == nil {
						report = payers.BuildUnableToDetermineReport(t.appointment)
					}
				}
			}
		}

		status := apptStatus(report)
		summary.RecordAppointment(t.appointment, status)
		log.Printf("[NewPayer] result patNum=%s aptNum=%s status=%s", t.appointment.PatNum, t.appointment.AptNum, status)

		if writer != nil {
			writer.ApplyResult(t.appointment, status, input.RequestedOfficeKey, nil, false)
		}
		input.QueuePDFTask(t.appointment, status, report, outputDir)
	}

	return summary, nil
}

// runPhase2Only is used when skipProbing=true — reads existing probe files from disk.
func (a *Adapter) runPhase2Only(ctx context.Context, input payers.SessionInput) (payers.RunSummary, error) {
	var summary payers.RunSummary
	outputDir := filepath.Join("artifacts", "results")
	if err := os.MkdirAll(outputDir, 0o755); err != nil {
		return summary, fmt.Errorf("NewPayer create results dir: %w", err)
	}

	appointments := input.Appointments
	if len(appointments) == 0 {
		appointments = payers.ScanProbeAppointments(input.ProbeOutputDir, PayerURL, probeFileSuffix)
		log.Printf("[NewPayer] skipProbing bucket scan: %d probe files in %s", len(appointments), input.ProbeOutputDir)
	}
	if len(appointments) == 0 {
		return summary, nil
	}

	writer, writerErr := resultwriter.New(input.Testing, input.ScraperConfig.APIs)
	if writerErr != nil {
		log.Printf("[NewPayer] resultwriter unavailable: %v", writerErr)
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

		probePath := payers.ProbeFilePathForAppointment(input.ProbeOutputDir, PayerURL, appt, probeFileSuffix)
		raw, rerr := readProbeResult(probePath)

		var report *advanced.PatientEligibilityReport
		statusOverride := ""

		if rerr != nil {
			log.Printf("[NewPayer] skipProbing read failed patNum=%s: %v", appt.PatNum, rerr)
			if probeErr, perr := payers.ReadProbeErrorForAppointment(input.ProbeOutputDir, PayerURL, appt); perr == nil {
				statusOverride = resultwriter.StatusForProbeErrorType(probeErr.ErrorType)
			}
			report = payers.BuildUnableToDetermineReport(appt)
		} else {
			el := newpayereligibility.Build(raw)
			if el == nil {
				report = payers.BuildUnableToDetermineReport(appt)
			} else if !el.Patient.IsEligible {
				r := payers.BuildNotActiveReport(appt, el.Plan.PlanName, el.Plan.Carrier, el.Plan.GroupName)
				r.Patient.MemberID = el.Patient.MemberID
				r.Patient.FullName = el.Patient.FullName
				report = r
			} else {
				report = advanced.Build(el, input.OfficeCodes, tpCodes)
				if report == nil {
					report = payers.BuildUnableToDetermineReport(appt)
				}
			}
		}

		status := apptStatus(report)
		if statusOverride != "" {
			status = statusOverride
		}
		summary.RecordAppointment(appt, status)
		log.Printf("[NewPayer] skipProbing result patNum=%s aptNum=%s status=%s", appt.PatNum, appt.AptNum, status)
		if writer != nil {
			writer.ApplyResult(appt, status, input.RequestedOfficeKey, nil, false)
		}
		input.QueuePDFTask(appt, status, report, outputDir)
	}

	return summary, nil
}

// ── probe file helpers ────────────────────────────────────────────────────────

// probeFileSuffix identifies probe files on disk for this payer.
// TODO(newpayer): change this string if you want a more descriptive suffix.
const probeFileSuffix = "probe"

// RawProbeData is whatever your scraper returns per patient.
// TODO(newpayer): replace this with a real struct that holds all scraped fields.
type RawProbeData struct {
	// Add fields here — e.g. MemberID, PlanName, EligibilityStatus, Benefits…
	Raw map[string]any `json:"raw"`
}

func writeProbeResult(dir string, appt models.Appointment, data *RawProbeData) (string, error) {
	path := payers.ProbeFilePathForAppointment(dir, PayerURL, appt, probeFileSuffix)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", err
	}
	b, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return "", err
	}
	return path, os.WriteFile(path, b, 0o644)
}

func readProbeResult(path string) (*RawProbeData, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var data RawProbeData
	return &data, json.Unmarshal(b, &data)
}

func writeProbeError(dir string, appt models.Appointment, probeErr error) {
	path := payers.ProbeFilePathForAppointment(dir, PayerURL, appt, "probe_error")
	_ = os.MkdirAll(filepath.Dir(path), 0o755)
	payload := map[string]any{
		"recordedAt":  time.Now().UTC().Format(time.RFC3339),
		"appointment": appt,
		"error":       probeErr.Error(),
		"errorType":   payers.ClassifyProbeError(probeErr),
	}
	b, _ := json.MarshalIndent(payload, "", "  ")
	_ = os.WriteFile(path, b, 0o644)
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

func sanitize(value string) string {
	var b strings.Builder
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	return strings.Trim(b.String(), "-")
}
