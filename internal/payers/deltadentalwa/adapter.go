package deltadentalwa

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"insurance-benefit-agent-go/internal/advanced"
	"insurance-benefit-agent-go/internal/controlplane"
	"insurance-benefit-agent-go/internal/eligibility"
	"insurance-benefit-agent-go/internal/models"
	"insurance-benefit-agent-go/internal/payers"
	ddapi "insurance-benefit-agent-go/internal/payers/deltadentalwa/api"
	ddbrowser "insurance-benefit-agent-go/internal/payers/deltadentalwa/browser"
	ddeligibility "insurance-benefit-agent-go/internal/payers/deltadentalwa/eligibility"
	"insurance-benefit-agent-go/internal/resultwriter"
)

const PayerURL = "deltadentalwa.com"

type Adapter struct {
	control *controlplane.Client
}

func NewAdapter(control *controlplane.Client) *Adapter {
	return &Adapter{control: control}
}

func (a *Adapter) PayerURL() string { return PayerURL }

func (a *Adapter) Supports(payerURL string) bool {
	return strings.EqualFold(strings.TrimSpace(payerURL), "deltadentalwa.com")
}

func (a *Adapter) Run(ctx context.Context, input payers.SessionInput) (payers.RunSummary, error) {
	_, _ = ctx, a.control
	var summary payers.RunSummary
	if !a.Supports(input.Payer.PayerURL) {
		return summary, fmt.Errorf("Delta Dental WA adapter does not support payerUrl=%s", input.Payer.PayerURL)
	}
	if input.SkipProbing {
		return a.runPhase2Only(ctx, input)
	}
	if len(input.Appointments) == 0 {
		return summary, fmt.Errorf("Delta Dental WA session requires at least one appointment")
	}

	runStamp := time.Now().UTC().Format("2006-01-02T15-04-05Z")

	session, err := ddbrowser.Launch(input)
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
			log.Printf("[DeltaDentalWA] browser close failed: %v", closeErr)
		}
	}
	defer closeBrowser()

	if err := session.Login(input); err != nil {
		return summary, fmt.Errorf("Delta Dental WA login: %w", err)
	}

	hashCache := ddapi.LoadHashCache(input.RequestedOfficeKey)
	probe := ddapi.NewBrowserProbe(session, hashCache)

	outputDir := filepath.Join(
		"artifacts",
		sanitizeSegment(input.RequestedOfficeKey),
		runStamp,
		sanitizeSegment(input.Payer.PayerURL),
	)
	tempProbeDir := input.ProbeOutputDir
	if tempProbeDir == "" {
		tempProbeDir = filepath.Join(outputDir, "_tmp_probe")
	}
	if err := os.MkdirAll(tempProbeDir, 0o755); err != nil {
		return summary, fmt.Errorf("create Delta Dental WA temp probe dir: %w", err)
	}
	log.Printf("[DeltaDentalWA] keeping temp probe files in %s", tempProbeDir)

	type appointmentTask struct {
		appointment models.Appointment
		tpCodes     []string
		probePath   string
		report      *advanced.PatientEligibilityReport
	}
	var tasks []appointmentTask

	sessionRefreshed := false

	// Phase 1: browser open — search each patient, write probe files.
	for _, appointment := range input.Appointments {
		select {
		case <-ctx.Done():
			return summary, ctx.Err()
		default:
		}

		var tpCodes []string
		if appointment.TreatmentPlanProcCodes != "" {
			tpCodes = strings.Split(appointment.TreatmentPlanProcCodes, ",")
		}

		task := appointmentTask{appointment: appointment, tpCodes: tpCodes}

		bundle, probeErr := probe.SearchAndFetchPatient(appointment)
		if errors.Is(probeErr, ddapi.ErrSessionExpired) && !sessionRefreshed {
			// Stale session cookie — delete auth file and re-login once.
			log.Printf("[DeltaDentalWA] session expired, clearing auth and re-logging in")
			_ = os.Remove(session.StorageStatePath())
			if loginErr := session.Login(input); loginErr != nil {
				return summary, fmt.Errorf("Delta Dental WA re-login after session expiry: %w", loginErr)
			}
			sessionRefreshed = true
			bundle, probeErr = probe.SearchAndFetchPatient(appointment)
		}

		if probeErr != nil {
			log.Printf("[DeltaDentalWA] probe failed patNum=%s aptNum=%s: %v", appointment.PatNum, appointment.AptNum, probeErr)
			writeProbeError(tempProbeDir, appointment, probeErr)
			task.report = payers.BuildNotFoundReport(appointment)
		} else if bundle == nil {
			task.report = payers.BuildUnableToDetermineReport(appointment)
		} else if bundle.NotFound {
			log.Printf("[DeltaDentalWA] member not found patNum=%s aptNum=%s", appointment.PatNum, appointment.AptNum)
			task.report = payers.BuildNotFoundReport(appointment)
		} else {
			probePath, err := writeAPIBundle(tempProbeDir, appointment, bundle)
			if err != nil {
				log.Printf("[DeltaDentalWA] probe write failed patNum=%s aptNum=%s: %v", appointment.PatNum, appointment.AptNum, err)
			} else {
				task.probePath = probePath
				logProbeSummary(appointment, bundle, probePath)
			}
		}

		tasks = append(tasks, task)
	}

	closeBrowser()
	hashCache.Flush()
	log.Printf("[DeltaDentalWA] phase 1 complete; probe files in %s", tempProbeDir)

	writer, writerErr := resultwriter.New(input.Testing, input.ScraperConfig.APIs)
	if writerErr != nil {
		log.Printf("[DeltaDentalWA] resultwriter unavailable - apptField/PDF upload disabled: %v", writerErr)
	}

	// Phase 2: browser closed — read probes and build eligibility reports.
	for i := range tasks {
		task := &tasks[i]
		if task.report == nil && task.probePath != "" {
			bundle, readErr := readAPIBundle(task.probePath)
			if readErr != nil {
				log.Printf("[DeltaDentalWA] probe read failed patNum=%s aptNum=%s: %v",
					task.appointment.PatNum, task.appointment.AptNum, readErr)
				task.report = payers.BuildUnableToDetermineReport(task.appointment)
			} else {
				el := ddeligibility.BuildEligibilityFromProbeBundle(bundle)
				if el == nil {
					task.report = payers.BuildUnableToDetermineReport(task.appointment)
				} else if !el.Patient.IsEligible {
					r := payers.BuildNotActiveReport(task.appointment, el.Plan.PlanName, el.Plan.Carrier, el.Plan.GroupName)
					r.Patient.MemberID = el.Patient.MemberID
					r.Patient.GroupNumber = el.Patient.GroupNumber
					r.Patient.FullName = el.Patient.FullName
					task.report = r
				} else {
					task.report = advanced.Build(el, input.OfficeCodes, task.tpCodes)
					if task.report == nil {
						task.report = payers.BuildUnableToDetermineReport(task.appointment)
					}
				}
				if input.Testing.ShouldWriteDebugArtifacts() {
					writeEligibilityResult(outputDir, task.appointment, el, input)
					writeAdvancedResult(outputDir, task.appointment, task.report)
				}
			}
		}

		status := apptStatus(task.report)
		summary.RecordAppointment(task.appointment, status)
		log.Printf("[DeltaDentalWA] finalizing result patNum=%s aptNum=%s status=%s",
			task.appointment.PatNum, task.appointment.AptNum, status)
		if writer != nil {
			writer.ApplyResult(task.appointment, status, input.RequestedOfficeKey, nil, false)
		}
		input.QueuePDFTask(task.appointment, status, task.report, outputDir)
	}

	return summary, nil
}

func (a *Adapter) runPhase2Only(ctx context.Context, input payers.SessionInput) (payers.RunSummary, error) {
	var summary payers.RunSummary
	outputDir := filepath.Join("artifacts", "results")
	if err := os.MkdirAll(outputDir, 0o755); err != nil {
		return summary, fmt.Errorf("create Delta Dental WA results dir: %w", err)
	}

	appointments := input.Appointments
	if len(appointments) == 0 {
		appointments = scanProbeStubAppointments(input.ProbeOutputDir, PayerURL, "api_probe")
		log.Printf("[DeltaDentalWA] skipProbing bucket scan: found %d probe files in %s", len(appointments), input.ProbeOutputDir)
	}
	if len(appointments) == 0 {
		log.Printf("[DeltaDentalWA] skipProbing: no probe files found, nothing to postprocess")
		return summary, nil
	}
	log.Printf("[DeltaDentalWA] skipProbing=true reading probes from %s", input.ProbeOutputDir)

	writer, writerErr := resultwriter.New(input.Testing, input.ScraperConfig.APIs)
	if writerErr != nil {
		log.Printf("[DeltaDentalWA] resultwriter unavailable: %v", writerErr)
	}

	for _, appointment := range appointments {
		select {
		case <-ctx.Done():
			return summary, ctx.Err()
		default:
		}

		var tpCodes []string
		if appointment.TreatmentPlanProcCodes != "" {
			tpCodes = strings.Split(appointment.TreatmentPlanProcCodes, ",")
		}

		var report *advanced.PatientEligibilityReport
		statusOverride := ""

		probePath := payers.ProbeFilePathForAppointment(input.ProbeOutputDir, PayerURL, appointment, "api_probe")
		bundle, readErr := readAPIBundle(probePath)
		if readErr != nil {
			log.Printf("[DeltaDentalWA] skipProbing read failed patNum=%s aptNum=%s: %v", appointment.PatNum, appointment.AptNum, readErr)
			if probeErr, err := payers.ReadProbeErrorForAppointment(input.ProbeOutputDir, PayerURL, appointment); err == nil {
				statusOverride = resultwriter.StatusForProbeErrorType(probeErr.ErrorType)
				log.Printf("[DeltaDentalWA] probe error result patNum=%s aptNum=%s errorType=%s error=%q", appointment.PatNum, appointment.AptNum, probeErr.ErrorType, probeErr.Error)
			}
			report = payers.BuildUnableToDetermineReport(appointment)
		} else {
			el := ddeligibility.BuildEligibilityFromProbeBundle(bundle)
			if el == nil {
				report = payers.BuildUnableToDetermineReport(appointment)
			} else if !el.Patient.IsEligible {
				r := payers.BuildNotActiveReport(appointment, el.Plan.PlanName, el.Plan.Carrier, el.Plan.GroupName)
				r.Patient.MemberID = el.Patient.MemberID
				r.Patient.GroupNumber = el.Patient.GroupNumber
				r.Patient.FullName = el.Patient.FullName
				report = r
			} else {
				report = advanced.Build(el, input.OfficeCodes, tpCodes)
				if report == nil {
					report = payers.BuildUnableToDetermineReport(appointment)
				}
			}
			writeEligibilityResult(outputDir, appointment, el, input)
			writeAdvancedResult(outputDir, appointment, report)
		}

		status := apptStatus(report)
		if statusOverride != "" {
			status = statusOverride
		}
		summary.RecordAppointment(appointment, status)
		log.Printf("[DeltaDentalWA] skipProbing result patNum=%s aptNum=%s status=%s", appointment.PatNum, appointment.AptNum, status)
		if writer != nil {
			writer.ApplyResult(appointment, status, input.RequestedOfficeKey, nil, false)
		}
		input.QueuePDFTask(appointment, status, report, outputDir)
	}

	return summary, nil
}

func logProbeSummary(appointment models.Appointment, bundle *ddapi.PatientAPIBundle, probePath string) {
	if bundle == nil {
		return
	}
	name := ""
	active := false
	company := ""
	hasBenefits := bundle.Benefits != nil
	if bundle.MemberSearch != nil {
		name = bundle.MemberSearch.SubscriberFirstName + " " + bundle.MemberSearch.SubscriberLastName
		active = bundle.MemberSearch.ActiveStatus
		company = bundle.MemberSearch.MemberCompanyName
	}
	log.Printf("[DeltaDentalWA] probe summary patNum=%s aptNum=%s name=%q active=%v company=%q hasBenefits=%v file=%s",
		appointment.PatNum, appointment.AptNum, name, active, company, hasBenefits, probePath)
}

func writeAdvancedResult(outputDir string, appointment models.Appointment, report *advanced.PatientEligibilityReport) {
	if report == nil {
		return
	}
	filePath := filepath.Join(outputDir, fmt.Sprintf("%s_%s_advanced.json",
		sanitizeSegment(appointment.PatNum),
		sanitizeSegment(appointment.AptNum),
	))
	if err := os.MkdirAll(filepath.Dir(filePath), 0o755); err != nil {
		log.Printf("[DeltaDentalWA] create advanced artifact dir failed patNum=%s: %v", appointment.PatNum, err)
		return
	}
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		log.Printf("[DeltaDentalWA] marshal advanced failed patNum=%s: %v", appointment.PatNum, err)
		return
	}
	if err := os.WriteFile(filePath, data, 0o644); err != nil {
		log.Printf("[DeltaDentalWA] write advanced failed patNum=%s: %v", appointment.PatNum, err)
	}
}

func writeAPIBundle(outputDir string, appointment models.Appointment, bundle *ddapi.PatientAPIBundle) (string, error) {
	if bundle == nil {
		return "", fmt.Errorf("probe bundle is nil")
	}
	filePath := payers.ProbeFilePathForAppointment(outputDir, PayerURL, appointment, "api_probe")
	if err := os.MkdirAll(filepath.Dir(filePath), 0o755); err != nil {
		return "", fmt.Errorf("create probe artifact dir: %w", err)
	}
	data, err := json.MarshalIndent(bundle, "", "  ")
	if err != nil {
		return "", fmt.Errorf("marshal probe artifact: %w", err)
	}
	if err := os.WriteFile(filePath, data, 0o644); err != nil {
		return "", err
	}
	return filePath, nil
}

func readAPIBundle(path string) (*ddapi.PatientAPIBundle, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read probe bundle: %w", err)
	}
	var bundle ddapi.PatientAPIBundle
	if err := json.Unmarshal(data, &bundle); err != nil {
		return nil, fmt.Errorf("unmarshal probe bundle: %w", err)
	}
	return &bundle, nil
}

func writeProbeError(outputDir string, appointment models.Appointment, probeErr error) {
	filePath := payers.ProbeFilePathForAppointment(outputDir, PayerURL, appointment, "probe_error")
	if err := os.MkdirAll(filepath.Dir(filePath), 0o755); err != nil {
		log.Printf("[DeltaDentalWA] create temp probe error dir failed patNum=%s: %v", appointment.PatNum, err)
		return
	}
	payload := map[string]any{
		"recordedAt":  time.Now().UTC().Format(time.RFC3339),
		"appointment": appointment,
		"error":       probeErr.Error(),
		"errorType":   payers.ClassifyProbeError(probeErr),
	}
	data, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		log.Printf("[DeltaDentalWA] marshal temp probe error failed patNum=%s: %v", appointment.PatNum, err)
		return
	}
	if err := os.WriteFile(filePath, data, 0o644); err != nil {
		log.Printf("[DeltaDentalWA] write temp probe error failed patNum=%s: %v", appointment.PatNum, err)
	}
}

func writeEligibilityResult(outputDir string, appointment models.Appointment, el *eligibility.PatientEligibility, input payers.SessionInput) {
	filePath := filepath.Join(outputDir, fmt.Sprintf("%s_%s_eligibility.json",
		sanitizeSegment(appointment.PatNum),
		sanitizeSegment(appointment.AptNum),
	))
	if err := os.MkdirAll(filepath.Dir(filePath), 0o755); err != nil {
		log.Printf("[DeltaDentalWA] create eligibility artifact dir failed patNum=%s: %v", appointment.PatNum, err)
		return
	}
	payload := map[string]any{
		"recordedAt":         time.Now().UTC().Format(time.RFC3339),
		"officeKey":          input.RequestedOfficeKey,
		"payerUrl":           input.Payer.PayerURL,
		"aptNum":             appointment.AptNum,
		"patNum":             appointment.PatNum,
		"patientEligibility": el,
	}
	data, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		log.Printf("[DeltaDentalWA] marshal eligibility failed patNum=%s: %v", appointment.PatNum, err)
		return
	}
	if err := os.WriteFile(filePath, data, 0o644); err != nil {
		log.Printf("[DeltaDentalWA] write eligibility failed patNum=%s: %v", appointment.PatNum, err)
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

func scanProbeStubAppointments(probeDir, payerURL, suffix string) []models.Appointment {
	prefix := payers.SanitizeProbeSegment(payerURL) + "_"
	sfx := "_" + suffix + ".json"
	matches, _ := filepath.Glob(filepath.Join(probeDir, prefix+"*"+sfx))
	var result []models.Appointment
	for _, f := range matches {
		base := strings.TrimSuffix(filepath.Base(f), sfx)
		base = strings.TrimPrefix(base, prefix)
		idx := strings.Index(base, "_")
		if idx < 0 {
			continue
		}
		result = append(result, models.Appointment{
			PatNum: base[:idx],
			AptNum: base[idx+1:],
		})
	}
	return result
}

var reUnsafe = regexp.MustCompile(`[<>:"/\\|?*\s]+`)

func sanitizeSegment(value string) string {
	return strings.Trim(reUnsafe.ReplaceAllString(value, "-"), "-")
}
