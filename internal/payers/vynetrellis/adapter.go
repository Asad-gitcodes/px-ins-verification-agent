package vynetrellis

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"insurance-benefit-agent-go/internal/advanced"
	"insurance-benefit-agent-go/internal/models"
	"insurance-benefit-agent-go/internal/payers"
	vtapi "insurance-benefit-agent-go/internal/payers/vynetrellis/api"
	vteligibility "insurance-benefit-agent-go/internal/payers/vynetrellis/eligibility"
	"insurance-benefit-agent-go/internal/resultwriter"
)

const PayerURL = "VyneTrellis.com"

type Adapter struct{}

func NewAdapter() *Adapter { return &Adapter{} }

func (a *Adapter) PayerURL() string              { return PayerURL }
func (a *Adapter) Supports(payerURL string) bool { return strings.EqualFold(payerURL, PayerURL) }

func (a *Adapter) Run(ctx context.Context, input payers.SessionInput) (payers.RunSummary, error) {
	if input.SkipProbing {
		return a.runPhase2Only(ctx, input)
	}
	return a.runPhase1(ctx, input)
}

// ── Phase 1: login → search patient → check eligibility → write probe ─────────

func (a *Adapter) runPhase1(ctx context.Context, input payers.SessionInput) (payers.RunSummary, error) {
	var summary payers.RunSummary

	client := vtapi.NewClient()
	if err := client.Login(input.Credential.Username, input.Credential.Password); err != nil {
		return summary, fmt.Errorf("[VyneTrellis] login failed: %w", err)
	}
	if err := client.FetchPracticeInfo(); err != nil {
		log.Printf("[VyneTrellis] practice info unavailable (provider NPI will be empty): %v", err)
	} else if p := client.PracticeInfo(); p != nil {
		log.Printf("[VyneTrellis] practice info loaded provider=%s %s NPI=%s", p.ProviderFirstName, p.ProviderLastName, p.ProviderNPI)
	}
	log.Printf("[VyneTrellis] login OK appointments=%d", len(input.Appointments))

	probeDir := input.ProbeOutputDir
	if probeDir == "" {
		probeDir = payers.ProbeRunDir("")
	}
	if err := os.MkdirAll(probeDir, 0o755); err != nil {
		return summary, fmt.Errorf("[VyneTrellis] create probe dir: %w", err)
	}

	for _, appt := range input.Appointments {
		select {
		case <-ctx.Done():
			return summary, ctx.Err()
		default:
		}

		if err := a.processAppointment(ctx, client, appt, input, probeDir); err != nil {
			log.Printf("[VyneTrellis] probe failed patNum=%s aptNum=%s: %v", appt.PatNum, appt.AptNum, err)
			writeProbeError(probeDir, input.Payer.PayerURL, appt, err)
		}
	}

	log.Printf("[VyneTrellis] phase 1 done; probe files in %s", probeDir)
	return summary, nil
}

func (a *Adapter) processAppointment(_ context.Context, client *vtapi.Client, appt models.Appointment, input payers.SessionInput, probeDir string) error {
	detail := buildDetailFromAppointment(appt, client.CustomerID())

	if p := client.PracticeInfo(); p != nil {
		detail.IndividualNpi = p.ProviderNPI
		detail.TaxonomyCode = p.TaxonomyCode
		detail.ProviderFirstName = p.ProviderFirstName
		detail.ProviderLastName = p.ProviderLastName
	}

	// Normalize carrier through carriers.json so the verify request uses the
	// canonical ID+name Vyne expects, based on the appointment's PayerID.
	if appt.PayerID != "" {
		resolvedID, resolvedName := ResolveCarrier(appt.PayerID, detail.CarrierName)
		if resolvedID != detail.CarrierId || resolvedName != detail.CarrierName {
			log.Printf("[VyneTrellis] carrier normalized patNum=%s: %s/%q → %s/%q",
				appt.PatNum, detail.CarrierId, detail.CarrierName, resolvedID, resolvedName)
		} else {
			log.Printf("[VyneTrellis] carrier confirmed patNum=%s: %s/%q", appt.PatNum, resolvedID, resolvedName)
		}
		detail.CarrierId = resolvedID
		detail.CarrierName = resolvedName
	} else {
		log.Printf("[VyneTrellis] carrier from appointment patNum=%s: %q", appt.PatNum, detail.CarrierName)
	}

	// Step 1: search Vyne for the patient to get their UUID SyncId.
	var storedPatient *vtapi.PatientDetail
	if match := client.FindPatient(detail.PatientLastName, detail.PatientFirstName, detail.PatientBirthdate); match != nil {
		detail.SyncId = match.SyncId
		log.Printf("[VyneTrellis] resolved Vyne syncId=%s for patNum=%s", match.SyncId, appt.PatNum)
		if p, getErr := client.GetPatient(match.PatientId); getErr == nil {
			storedPatient = p
		}
	} else {
		log.Printf("[VyneTrellis] no Vyne patient found for patNum=%s", appt.PatNum)
	}

	// Step 2: always call verify/0 for fresh eligibility.
	log.Printf("[VyneTrellis] checking eligibility patNum=%s aptNum=%s subscriber=%s %s carrier=%q",
		appt.PatNum, appt.AptNum, detail.SubscriberFirstName, detail.SubscriberLastName, detail.CarrierName)
	verification, err := client.CheckEligibility(detail)
	if err != nil {
		log.Printf("[VyneTrellis] eligibility check failed patNum=%s: %v", appt.PatNum, err)
	}

	// Step 3: if verify returned empty, fall back to VerificationHistory within 7 days.
	if (verification == nil || (verification.StatusCode == 0 && verification.Status == "")) && storedPatient != nil {
		if fallback := recentHistoryStatus(storedPatient.VerificationHistory, 7*24*time.Hour); fallback != "" {
			verification = &vtapi.VerifyResponse{Status: fallback}
			log.Printf("[VyneTrellis] using history fallback status=%q for patNum=%s", fallback, appt.PatNum)
		}
	}
	if err != nil && (verification == nil || (verification.StatusCode == 0 && verification.Status == "")) {
		return err
	}

	bundle := &vtapi.ProbeBundle{
		OriginalPayerID: appt.PayerID,
		Patient:         detail,
		Verification:    verification,
	}
	return writeProbeBundle(probeDir, appt, input.Payer.PayerURL, bundle)
}

// buildDetailFromAppointment constructs a PatientDetail from OD appointment data,
// ready to pass directly to CheckEligibility without a prior Vyne search.
func buildDetailFromAppointment(appt models.Appointment, customerID int) *vtapi.PatientDetail {
	patientIsSub := appt.Relationship == "0" || appt.Relationship == ""

	subFName := strings.TrimSpace(appt.SubFName)
	subLName := strings.TrimSpace(appt.SubLName)
	subDOB := strings.TrimSpace(appt.SubDOB)
	if subFName == "" {
		subFName = strings.TrimSpace(appt.FName)
	}
	if subLName == "" {
		subLName = strings.TrimSpace(appt.LName)
	}
	if subDOB == "" {
		subDOB = strings.TrimSpace(appt.DOB)
	}

	patGender := mapODGender(appt.Gender)
	subGender := mapODGender(appt.SubGender)
	if patientIsSub && subGender == "" {
		subGender = patGender
	}

	return &vtapi.PatientDetail{
		CustomerId:          customerID,
		SyncId:              strings.TrimSpace(appt.PatNum),
		PatientFirstName:    strings.TrimSpace(appt.FName),
		PatientLastName:     strings.TrimSpace(appt.LName),
		PatientBirthdate:    normalizeDOBForVyne(appt.DOB),
		PatientGender:       patGender,
		PatientIsSub:        patientIsSub,
		SubscriberId:        strings.TrimSpace(appt.SubscriberID),
		SubscriberFirstName: subFName,
		SubscriberLastName:  subLName,
		SubscriberBirthdate: normalizeDOBForVyne(subDOB),
		SubscriberGender:    subGender,
		CarrierName:         strings.TrimRight(strings.TrimSpace(appt.CarrierName), ","),
		GroupNumber:         strings.TrimSpace(appt.GroupNum),
	}
}

// mapODGender converts Open Dental gender enum (0=Male, 1=Female) to Vyne's "M"/"F".
func mapODGender(g string) string {
	switch strings.TrimSpace(g) {
	case "1":
		return "F"
	case "0":
		return "M"
	default:
		return ""
	}
}

// normalizeDOBForVyne converts OD date format (MM-DD-YYYY) to Vyne's expected MM/DD/YYYY.
func normalizeDOBForVyne(dob string) string {
	dob = strings.TrimSpace(dob)
	layouts := []struct{ in, out string }{
		{"01-02-2006", "01/02/2006"},
		{"01/02/2006", "01/02/2006"},
		{"2006-01-02", "01/02/2006"},
	}
	for _, l := range layouts {
		if t, err := time.Parse(l.in, dob); err == nil {
			return t.Format(l.out)
		}
	}
	return dob
}

// ── Phase 2: read probe → build eligibility → PDF ─────────────────────────────

func (a *Adapter) runPhase2Only(ctx context.Context, input payers.SessionInput) (payers.RunSummary, error) {
	var summary payers.RunSummary

	outputDir := "artifacts/results"
	if err := os.MkdirAll(outputDir, 0o755); err != nil {
		return summary, fmt.Errorf("[VyneTrellis] create results dir: %w", err)
	}

	appointments := input.Appointments
	if len(appointments) == 0 {
		appointments = payers.ScanProbeAppointments(input.ProbeOutputDir, input.Payer.PayerURL, "api_probe")
		log.Printf("[VyneTrellis] probe scan found %d appointments in %s", len(appointments), input.ProbeOutputDir)
	}
	if len(appointments) == 0 {
		log.Printf("[VyneTrellis] no probe files found, nothing to postprocess")
		return summary, nil
	}

	writer, writerErr := resultwriter.New(input.Testing, input.ScraperConfig.APIs)
	if writerErr != nil {
		log.Printf("[VyneTrellis] resultwriter unavailable: %v", writerErr)
	}

	for _, appt := range appointments {
		select {
		case <-ctx.Done():
			return summary, ctx.Err()
		default:
		}

		// VyneTrellis returns HTML-only eligibility with no per-code coverage data,
		// so the PDF should not show treatment or office-code rows.
		var reportCodes []string

		probePath := payers.ProbeFilePathForAppointment(input.ProbeOutputDir, input.Payer.PayerURL, appt, "api_probe")
		bundle, readErr := readProbeBundle(probePath)

		var report *advanced.PatientEligibilityReport
		statusOverride := ""
		if readErr != nil {
			log.Printf("[VyneTrellis] probe read failed patNum=%s aptNum=%s: %v", appt.PatNum, appt.AptNum, readErr)
			if probeErr, err := payers.ReadProbeErrorForAppointment(input.ProbeOutputDir, input.Payer.PayerURL, appt); err == nil {
				statusOverride = resultwriter.StatusForProbeErrorType(probeErr.ErrorType)
				log.Printf("[VyneTrellis] probe error result patNum=%s aptNum=%s errorType=%s error=%q", appt.PatNum, appt.AptNum, probeErr.ErrorType, probeErr.Error)
			}
			report = payers.BuildUnableToDetermineReport(appt)
		} else {
			el := vteligibility.BuildEligibilityFromProbe(bundle)
			if el == nil {
				report = payers.BuildUnableToDetermineReport(appt)
			} else if !el.Patient.IsEligible {
				r := payers.BuildNotActiveReport(appt, el.Plan.PlanName, el.Plan.Carrier, el.Plan.GroupName)
				r.Patient.FullName = el.Patient.FullName
				r.Patient.MemberID = el.Patient.MemberID
				report = r
			} else {
				report = advanced.Build(el, reportCodes, nil)
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
		log.Printf("[VyneTrellis] result patNum=%s aptNum=%s carrier=%s status=%s",
			appt.PatNum, appt.AptNum, carrierFromBundle(bundle), status)

		if writer != nil {
			writer.ApplyResult(appt, status, input.RequestedOfficeKey, nil, false)
		}
		input.QueuePDFTask(appt, status, report, outputDir)
	}

	return summary, nil
}

// ── probe file I/O ────────────────────────────────────────────────────────────

func writeProbeBundle(dir string, appt models.Appointment, payerURL string, bundle *vtapi.ProbeBundle) error {
	path := payers.ProbeFilePathForAppointment(dir, payerURL, appt, "api_probe")
	data, err := json.MarshalIndent(bundle, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal probe: %w", err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("write probe: %w", err)
	}
	log.Printf("[VyneTrellis] probe written patNum=%s aptNum=%s path=%s", appt.PatNum, appt.AptNum, path)
	return nil
}

func readProbeBundle(path string) (*vtapi.ProbeBundle, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read probe: %w", err)
	}
	var bundle vtapi.ProbeBundle
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
		log.Printf("[VyneTrellis] marshal results %s: %v", filename, err)
		return
	}
	if err := os.WriteFile(dir+"/"+filename, data, 0o644); err != nil {
		log.Printf("[VyneTrellis] write results %s: %v", filename, err)
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

func carrierFromBundle(bundle *vtapi.ProbeBundle) string {
	if bundle == nil || bundle.Patient == nil {
		return ""
	}
	return strings.TrimRight(strings.TrimSpace(bundle.Patient.CarrierName), ",")
}

// recentHistoryStatus returns the Status from the most recent VerificationHistory entry
// if its RequestDate is within maxAge. Returns "" if no qualifying entry exists.
func recentHistoryStatus(history []vtapi.VerificationRecord, maxAge time.Duration) string {
	layouts := []string{
		"01/02/2006 03:04:05.000 PM",
		"01/02/2006 3:04:05.000 PM",
		"01/02/2006",
	}
	cutoff := time.Now().UTC().Add(-maxAge)
	for _, rec := range history {
		if rec.Status == nil || *rec.Status == "" {
			continue
		}
		d := strings.TrimSpace(rec.RequestDate)
		if d == "" {
			continue
		}
		var t time.Time
		for _, layout := range layouts {
			if parsed, err := time.Parse(layout, d); err == nil {
				t = parsed
				break
			}
		}
		if t.IsZero() || t.Before(cutoff) {
			continue
		}
		return *rec.Status
	}
	return ""
}
