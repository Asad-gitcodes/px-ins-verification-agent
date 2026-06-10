package dentaquest

import (
	"context"
	"encoding/json"
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
	"insurance-benefit-agent-go/internal/logging"
	"insurance-benefit-agent-go/internal/models"
	"insurance-benefit-agent-go/internal/payers"
	dqapi "insurance-benefit-agent-go/internal/payers/dentaquest/api"
	dqbrowser "insurance-benefit-agent-go/internal/payers/dentaquest/browser"
	"insurance-benefit-agent-go/internal/resultwriter"
)

const PayerURL = "DentaQuest.com"

type Adapter struct {
	control *controlplane.Client
}

func NewAdapter(control *controlplane.Client) *Adapter {
	return &Adapter{control: control}
}

func (a *Adapter) PayerURL() string { return PayerURL }

func (a *Adapter) Supports(payerURL string) bool {
	return strings.EqualFold(payerURL, PayerURL)
}

func (a *Adapter) Run(ctx context.Context, input payers.SessionInput) (payers.RunSummary, error) {
	_, _ = ctx, a.control
	var summary payers.RunSummary
	if !a.Supports(input.Payer.PayerURL) {
		return summary, fmt.Errorf("DentaQuest adapter does not support payerUrl=%s", input.Payer.PayerURL)
	}
	if input.SkipProbing {
		return a.runPhase2Only(ctx, input)
	}
	if len(input.Appointments) == 0 {
		return summary, fmt.Errorf("DentaQuest session requires at least one appointment")
	}

	runStamp := time.Now().UTC().Format("2006-01-02T15-04-05Z")

	session, err := dqbrowser.Launch(input)
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
			log.Printf("[DentaQuest] browser close failed: %v", closeErr)
		}
	}
	defer closeBrowser()

	if err := session.Login(input); err != nil {
		logging.Error("dentaquest", "dentaquest.login.failed", "payer login failed", map[string]any{
			"error": err.Error(),
		})
		return summary, err
	}
	logging.Info("dentaquest", "dentaquest.login.completed", "payer login completed", nil)

	if strings.TrimSpace(input.Credential.ProviderName) != "" {
		if err := dqbrowser.SelectDashboardProvider(session.Page(), input.Credential.ProviderName); err != nil {
			return summary, fmt.Errorf("select DentaQuest provider %q: %w", input.Credential.ProviderName, err)
		}
	}

	probe := dqapi.NewBrowserProbe(session)
	baseContext, err := probe.DiscoverPracticeContext(normalizeProbeDate(input.Appointments[0].AppointmentDate), input.Credential.ProviderName)
	if err != nil {
		return summary, fmt.Errorf("discover DentaQuest practice context: %w", err)
	}
	logging.Info("dentaquest", "dentaquest.practice_context.discovered", "discovered DentaQuest practice context", map[string]any{
		"location":      baseContext.ServiceLocation,
		"practitioner":  baseContext.PractitionerName,
		"accessPointId": baseContext.AccessPointID,
		"routeId":       baseContext.RouteID,
	})

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
		return summary, fmt.Errorf("create DentaQuest temp probe dir: %w", err)
	}
	log.Printf("[DentaQuest] keeping temp probe files in %s", tempProbeDir)

	type appointmentTask struct {
		appointment models.Appointment
		tpCodes     []string
		probePath   string
		report      *advanced.PatientEligibilityReport // set when probe fails; bypasses bundle processing
	}
	var tasks []appointmentTask

	// Phase 1: browser open — scrape all patients and write probe files.
	for _, appointment := range input.Appointments {
		select {
		case <-ctx.Done():
			return summary, ctx.Err()
		default:
		}

		appointmentContext := *baseContext
		if normalizedDate := normalizeProbeDate(appointment.AppointmentDate); normalizedDate != "" {
			appointmentContext.DateOfService = normalizedDate
		}

		var tpCodes []string
		if appointment.TreatmentPlanProcCodes != "" {
			tpCodes = strings.Split(appointment.TreatmentPlanProcCodes, ",")
		}

		task := appointmentTask{appointment: appointment, tpCodes: tpCodes}

		bundle, err := probe.SearchAndFetchPatient(appointmentContext, appointment)
		if err != nil {
			log.Printf("[DentaQuest] probe failed patNum=%s aptNum=%s: %v", appointment.PatNum, appointment.AptNum, err)
			logging.Error("dentaquest", "dentaquest.probe.failed", "DentaQuest probe failed", map[string]any{
				"patNum": appointment.PatNum,
				"aptNum": appointment.AptNum,
				"error":  err.Error(),
			})
			writeProbeError(tempProbeDir, appointment, err)
			task.report = payers.BuildNotFoundReport(appointment)
		} else if bundle == nil {
			log.Printf("[DentaQuest] probe returned nil bundle patNum=%s aptNum=%s", appointment.PatNum, appointment.AptNum)
			task.report = payers.BuildUnableToDetermineReport(appointment)
		} else {
			probePath, err := writeAPIBundleResult(tempProbeDir, appointment, bundle)
			if err != nil {
				log.Printf("[DentaQuest] probe write failed patNum=%s aptNum=%s: %v", appointment.PatNum, appointment.AptNum, err)
			} else {
				task.probePath = probePath
				logDentaQuestProbeSummary(appointment, bundle, probePath)
			}
		}

		tasks = append(tasks, task)
	}

	closeBrowser()
	log.Printf("[DentaQuest] phase 2 paused; raw probe files kept in %s", tempProbeDir)
	return summary, nil

	writer, writerErr := resultwriter.New(input.Testing, input.ScraperConfig.APIs)
	if writerErr != nil {
		log.Printf("[DentaQuest] resultwriter unavailable - apptField/PDF upload disabled: %v", writerErr)
	}

	for i := range tasks {
		task := &tasks[i]
		if task.report == nil && task.probePath != "" {
			bundle, readErr := readAPIBundleResult(task.probePath)
			if readErr != nil {
				log.Printf("[DentaQuest] probe read failed patNum=%s aptNum=%s: %v",
					task.appointment.PatNum, task.appointment.AptNum, readErr)
				task.report = payers.BuildUnableToDetermineReport(task.appointment)
			} else {
				el := dqapi.BuildEligibilityFromProbeBundle(bundle)
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
					writeEligibilityResult(task.appointment, el, input, runStamp)
					writeAdvancedResult(task.appointment, task.report, input, runStamp)
				}
				logEligibilityOutcome(task.appointment, el, bundle)
			}
		}

		status := apptStatus(task.report)
		summary.RecordAppointment(task.appointment, status)
		log.Printf("[DentaQuest] finalizing result patNum=%s aptNum=%s status=%s",
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
		return summary, fmt.Errorf("create DentaQuest results dir: %w", err)
	}

	appointments := input.Appointments
	if len(appointments) == 0 {
		appointments = scanProbeStubAppointments(input.ProbeOutputDir, PayerURL, "api_probe")
		log.Printf("[DentaQuest] skipProbing bucket scan: found %d probe files in %s", len(appointments), input.ProbeOutputDir)
	}
	if len(appointments) == 0 {
		log.Printf("[DentaQuest] skipProbing: no probe files found, nothing to postprocess")
		return summary, nil
	}
	log.Printf("[DentaQuest] skipProbing=true reading probes from %s", input.ProbeOutputDir)

	writer, writerErr := resultwriter.New(input.Testing, input.ScraperConfig.APIs)
	if writerErr != nil {
		log.Printf("[DentaQuest] resultwriter unavailable: %v", writerErr)
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
		bundle, readErr := readAPIBundleResult(probePath)
		if readErr != nil {
			log.Printf("[DentaQuest] skipProbing read failed patNum=%s aptNum=%s: %v", appointment.PatNum, appointment.AptNum, readErr)
			if probeErr, err := payers.ReadProbeErrorForAppointment(input.ProbeOutputDir, PayerURL, appointment); err == nil {
				statusOverride = resultwriter.StatusForProbeErrorType(probeErr.ErrorType)
				log.Printf("[DentaQuest] probe error result patNum=%s aptNum=%s errorType=%s error=%q", appointment.PatNum, appointment.AptNum, probeErr.ErrorType, probeErr.Error)
			}
			report = payers.BuildUnableToDetermineReport(appointment)
		} else {
			el := dqapi.BuildEligibilityFromProbeBundle(bundle)
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
			writeResultsFile(outputDir, fmt.Sprintf("%s_%s_eligibility.json", sanitizeSegment(appointment.PatNum), sanitizeSegment(appointment.AptNum)), map[string]any{
				"recordedAt":         time.Now().UTC().Format(time.RFC3339),
				"officeKey":          input.RequestedOfficeKey,
				"payerUrl":           input.Payer.PayerURL,
				"aptNum":             appointment.AptNum,
				"patNum":             appointment.PatNum,
				"patientEligibility": el,
			})
			writeResultsFile(outputDir, fmt.Sprintf("%s_%s_advanced.json", sanitizeSegment(appointment.PatNum), sanitizeSegment(appointment.AptNum)), report)
			logEligibilityOutcome(appointment, el, bundle)
		}

		status := apptStatus(report)
		if statusOverride != "" {
			status = statusOverride
		}
		summary.RecordAppointment(appointment, status)
		log.Printf("[DentaQuest] skipProbing result patNum=%s aptNum=%s status=%s", appointment.PatNum, appointment.AptNum, status)
		if writer != nil {
			writer.ApplyResult(appointment, status, input.RequestedOfficeKey, nil, false)
		}
		input.QueuePDFTask(appointment, status, report, outputDir)
	}

	return summary, nil
}

func logDentaQuestProbeSummary(appointment models.Appointment, bundle *dqapi.PatientAPIBundle, probePath string) {
	if bundle == nil {
		return
	}
	detailErrors := 0
	searchResults := 0
	if bundle.ProbeDebug != nil {
		detailErrors = len(bundle.ProbeDebug.DetailFetchErrors)
		searchResults = bundle.ProbeDebug.SearchResultCount
	}
	sections := 0
	if bundle.MemberInfo != nil {
		sections++
	}
	if bundle.PlanInfo != nil {
		sections++
	}
	if bundle.Enrollment != nil {
		sections++
	}
	if bundle.Clinical != nil {
		sections++
	}
	if bundle.Benefit != nil {
		sections++
	}
	if bundle.Family != nil {
		sections++
	}
	if bundle.Maximum != nil {
		sections++
	}
	if bundle.COB != nil {
		sections++
	}
	if bundle.ClaimAuth != nil {
		sections++
	}
	if bundle.TreatmentPlan != nil {
		sections++
	}
	log.Printf("[DentaQuest] probe summary patNum=%s aptNum=%s memberId=%s searchResults=%d sections=%d detailErrors=%d file=%s",
		appointment.PatNum, appointment.AptNum, bundle.SearchResult.MemberID, searchResults, sections, detailErrors, probePath)
}

func writeAPIBundleResult(outputDir string, appointment models.Appointment, bundle *dqapi.PatientAPIBundle) (string, error) {
	if bundle == nil {
		return "", fmt.Errorf("probe result is nil")
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
		return "", fmt.Errorf("write probe artifact: %w", err)
	}

	logging.Info("dentaquest", "dentaquest.probe.artifact_written", "wrote DentaQuest probe artifact", map[string]any{
		"patNum":   appointment.PatNum,
		"aptNum":   appointment.AptNum,
		"filePath": filePath,
	})
	return filePath, nil
}

func readAPIBundleResult(path string) (*dqapi.PatientAPIBundle, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read probe bundle: %w", err)
	}
	var bundle dqapi.PatientAPIBundle
	if err := json.Unmarshal(data, &bundle); err != nil {
		return nil, fmt.Errorf("unmarshal probe bundle: %w", err)
	}
	return &bundle, nil
}

func writeProbeError(outputDir string, appointment models.Appointment, probeErr error) {
	filePath := payers.ProbeFilePathForAppointment(outputDir, PayerURL, appointment, "probe_error")
	if err := os.MkdirAll(filepath.Dir(filePath), 0o755); err != nil {
		log.Printf("[DentaQuest] create temp probe error dir failed patNum=%s: %v", appointment.PatNum, err)
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
		log.Printf("[DentaQuest] marshal temp probe error failed patNum=%s: %v", appointment.PatNum, err)
		return
	}
	if err := os.WriteFile(filePath, data, 0o644); err != nil {
		log.Printf("[DentaQuest] write temp probe error failed patNum=%s: %v", appointment.PatNum, err)
	}
}

func writeAdvancedResult(appointment models.Appointment, report *advanced.PatientEligibilityReport, input payers.SessionInput, runStamp string) {
	if report == nil {
		return
	}

	dir := filepath.Join(
		"artifacts",
		sanitizeSegment(input.RequestedOfficeKey),
		runStamp,
		sanitizeSegment(input.Payer.PayerURL),
	)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		log.Printf("[DentaQuest] failed to create advanced artifact dir %s: %v", dir, err)
		return
	}

	filePath := filepath.Join(dir, fmt.Sprintf("%s_%s_advanced.json",
		sanitizeSegment(appointment.PatNum),
		sanitizeSegment(appointment.AptNum),
	))
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		log.Printf("[DentaQuest] failed to marshal advanced for patNum=%s aptNum=%s: %v", appointment.PatNum, appointment.AptNum, err)
		return
	}
	if err := os.WriteFile(filePath, data, 0o644); err != nil {
		log.Printf("[DentaQuest] failed to write advanced artifact %s: %v", filePath, err)
		return
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

func artifactDir(input payers.SessionInput, runStamp string) string {
	return filepath.Join(
		"artifacts",
		sanitizeSegment(input.RequestedOfficeKey),
		runStamp,
		sanitizeSegment(input.Payer.PayerURL),
	)
}

func normalizeProbeDate(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Now().UTC().Format("2006-01-02")
	}

	layouts := []string{
		"2006-01-02",
		"2006/01/02",
		"01-02-2006",
		"01/02/2006",
		"1/2/2006",
	}
	for _, layout := range layouts {
		if parsed, err := time.Parse(layout, value); err == nil {
			return parsed.Format("2006-01-02")
		}
	}
	return value
}

func logEligibilityOutcome(appointment models.Appointment, el *eligibility.PatientEligibility, bundle *dqapi.PatientAPIBundle) {
	if el == nil {
		return
	}

	probeErrors := map[string]any{}
	probeErrorCount := 0
	if bundle != nil && bundle.ProbeDebug != nil && len(bundle.ProbeDebug.DetailFetchErrors) > 0 {
		probeErrorCount = len(bundle.ProbeDebug.DetailFetchErrors)
		for key, value := range bundle.ProbeDebug.DetailFetchErrors {
			probeErrors[key] = value
		}
	}

	log.Printf(
		"[DentaQuest] eligibility ready patNum=%s aptNum=%s eligible=%t member=%q plan=%q probeErrors=%d",
		appointment.PatNum,
		appointment.AptNum,
		el.Patient.IsEligible,
		el.Patient.FullName,
		el.Plan.PlanName,
		probeErrorCount,
	)

	fields := map[string]any{
		"patNum":                appointment.PatNum,
		"aptNum":                appointment.AptNum,
		"appointmentDate":       appointment.AppointmentDate,
		"subscriberId":          appointment.SubscriberID,
		"probeDetailErrorCount": probeErrorCount,
		"probeDetailErrors":     probeErrors,
		"eligibility":           el,
		"networkMatrixRowCount": len(el.NetworkMatrix),
		"accumulatorCount":      len(el.Accumulators),
		"treatmentHistoryCodes": len(el.TreatmentHistory),
		"coverageCategoryCount": len(el.Coverage.Categories),
	}

	if probeErrorCount > 0 {
		logging.Warn("dentaquest", "dentaquest.eligibility.partial", "eligibility built with upstream detail fetch errors", fields)
		return
	}
	logging.Info("dentaquest", "dentaquest.eligibility.built", "eligibility built", fields)
}

// runBatch performs a complete search + per-member scrape for one batch of <=10.
func runBatch(ctx context.Context, session *dqbrowser.Session, input payers.SessionInput, runStamp string, writer *resultwriter.Writer, isFirstBatch bool) error {
	summary, err := session.PrepareInitialMemberSearch(input, isFirstBatch)
	if err != nil {
		return err
	}
	logging.Info("dentaquest", "dentaquest.batch.search_completed", "batch search completed", map[string]any{
		"rows":       summary.BatchSize,
		"successful": summary.SuccessCount,
		"notFound":   summary.NotFound,
		"errors":     summary.Errored,
	})

	if summary.SuccessCount == 0 {
		logging.Warn("dentaquest", "dentaquest.batch.no_successful_members", "no successful members in batch", nil)
		return nil
	}

	remainingAppointments := summary.SuccessfulAppointments
	if len(remainingAppointments) == 0 && summary.SuccessCount > 0 {
		limit := summary.SuccessCount
		if limit > len(input.Appointments) {
			limit = len(input.Appointments)
		}
		remainingAppointments = append(remainingAppointments, input.Appointments[:limit]...)
	}

	matchedCount := 0
	unmatchedCount := 0
	openFailureCount := 0
	for rowIndex := 0; rowIndex < summary.SuccessCount; rowIndex++ {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		outcome, err := processMemberAtGridRow(session, rowIndex, input, runStamp, writer, remainingAppointments)
		if err != nil {
			logging.Error("dentaquest", "dentaquest.member.processing_failed", "member processing failed", map[string]any{
				"rowIndex": rowIndex + 1,
				"error":    err.Error(),
			})
		} else {
			switch outcome.status {
			case "matched":
				matchedCount++
				if outcome.appointment != nil {
					remainingAppointments = removeAppointmentByAptNum(remainingAppointments, outcome.appointment.AptNum)
				}
			case "unmatched":
				unmatchedCount++
			case "open_failed":
				openFailureCount++
			}
		}

		if err := session.ReturnToEligibilitySearch(); err != nil {
			return fmt.Errorf("DentaQuest return to eligibility search: %w", err)
		}
	}

	logging.Info("dentaquest", "dentaquest.batch.reconciled", "reconciled batch row outcomes", map[string]any{
		"expected":   summary.SuccessCount,
		"matched":    matchedCount,
		"unmatched":  unmatchedCount,
		"openFailed": openFailureCount,
		"remaining":  len(remainingAppointments),
		"notFound":   summary.NotFound,
		"errors":     summary.Errored,
	})
	return nil
}

type rowProcessOutcome struct {
	status      string
	appointment *models.Appointment
}

func processMemberAtGridRow(session *dqbrowser.Session, rowIndex int, input payers.SessionInput, runStamp string, writer *resultwriter.Writer, remainingAppointments []models.Appointment) (rowProcessOutcome, error) {
	if err := session.OpenMemberDetailsFromGridIndex(rowIndex); err != nil {
		logging.Warn("dentaquest", "dentaquest.member.not_in_results_grid", "member not found in results grid", map[string]any{
			"rowIndex": rowIndex + 1,
			"error":    err.Error(),
		})
		return rowProcessOutcome{status: "open_failed"}, nil
	}

	el, err := session.ScrapeCurrentMemberDetails()
	if err != nil {
		return rowProcessOutcome{}, fmt.Errorf("scrape member details row=%d: %w", rowIndex+1, err)
	}

	matchedAppointment := matchScrapedEligibilityToAppointment(el, remainingAppointments)
	if matchedAppointment == nil {
		logging.Warn("dentaquest", "dentaquest.member.match_unresolved", "unable to match scraped member details to a batch appointment", map[string]any{
			"rowIndex":           rowIndex + 1,
			"fullName":           el.Patient.FullName,
			"dob":                el.Patient.DateOfBirth,
			"memberId":           el.Patient.MemberID,
			"batchCandidates":    len(remainingAppointments),
			"batchCandidateDOBs": distinctNormalizedDOBs(remainingAppointments),
		})
		writeUnmatchedEligibilityResult(rowIndex, el, input, runStamp)
		return rowProcessOutcome{status: "unmatched"}, nil
	}
	logging.Info("dentaquest", "dentaquest.member.matched", "matched scraped member details to appointment", map[string]any{
		"rowIndex": rowIndex + 1,
		"patNum":   matchedAppointment.PatNum,
		"aptNum":   matchedAppointment.AptNum,
		"fullName": el.Patient.FullName,
		"dob":      el.Patient.DateOfBirth,
		"matchBy":  "dob+lname_partial+fname_partial",
	})

	writeEligibilityResult(*matchedAppointment, el, input, runStamp)

	if writer != nil {
		status := resultwriter.EligibilityStatus(el.Patient.IsEligible)
		writer.ApplyResult(*matchedAppointment, status, input.RequestedOfficeKey, nil, input.WritePDF)
	}
	return rowProcessOutcome{status: "matched", appointment: matchedAppointment}, nil
}

func writeUnmatchedEligibilityResult(rowIndex int, el *eligibility.PatientEligibility, input payers.SessionInput, runStamp string) {
	dir := filepath.Join(
		"artifacts",
		sanitizeSegment(input.RequestedOfficeKey),
		runStamp,
		sanitizeSegment(input.Payer.PayerURL),
	)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		log.Printf("[DentaQuest] failed to create unmatched artifact dir %s: %v", dir, err)
		return
	}

	memberID := sanitizeSegment(el.Patient.MemberID)
	if memberID == "" {
		memberID = "unknown-member"
	}
	filename := fmt.Sprintf("unmatched-row-%d_%s_eligibility.json", rowIndex+1, memberID)
	filePath := filepath.Join(dir, filename)

	payload := map[string]any{
		"recordedAt":         time.Now().UTC().Format(time.RFC3339),
		"officeKey":          input.RequestedOfficeKey,
		"payerUrl":           input.Payer.PayerURL,
		"gridRowIndex":       rowIndex + 1,
		"matchStatus":        "unmatched",
		"patientEligibility": el,
	}

	data, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		log.Printf("[DentaQuest] failed to marshal unmatched eligibility artifact row=%d: %v", rowIndex+1, err)
		return
	}

	if err := os.WriteFile(filePath, data, 0o644); err != nil {
		log.Printf("[DentaQuest] failed to write unmatched artifact %s: %v", filePath, err)
		return
	}

	logging.Warn("dentaquest", "dentaquest.artifact.unmatched_written", "wrote unmatched eligibility artifact", map[string]any{
		"rowIndex": rowIndex + 1,
		"filePath": filePath,
		"fullName": el.Patient.FullName,
		"dob":      el.Patient.DateOfBirth,
		"memberId": el.Patient.MemberID,
	})
}

func matchScrapedEligibilityToAppointment(el *eligibility.PatientEligibility, appointments []models.Appointment) *models.Appointment {
	if el == nil {
		return nil
	}

	scrapedDOB := normalizeDateKey(el.Patient.DateOfBirth)
	scrapedFirst := firstNameKey(el.Patient.FullName)
	scrapedLast := lastNameKey(el.Patient.FullName)
	if scrapedDOB == "" || scrapedFirst == "" || scrapedLast == "" {
		return nil
	}

	dobMatches := make([]*models.Appointment, 0, len(appointments))
	for i := range appointments {
		appointment := &appointments[i]
		if normalizeDateKey(appointment.DOB) == scrapedDOB {
			dobMatches = append(dobMatches, appointment)
		}
	}
	if len(dobMatches) == 0 {
		return nil
	}

	nameMatches := make([]*models.Appointment, 0, len(dobMatches))
	for _, appointment := range dobMatches {
		if !partialNameMatch(scrapedLast, appointment.LName) {
			continue
		}
		if !partialNameMatch(scrapedFirst, appointment.FName) {
			continue
		}
		nameMatches = append(nameMatches, appointment)
	}
	if len(nameMatches) == 1 {
		return nameMatches[0]
	}

	return nil
}

func removeAppointmentByAptNum(appointments []models.Appointment, aptNum string) []models.Appointment {
	for i := range appointments {
		if appointments[i].AptNum == aptNum {
			return append(appointments[:i], appointments[i+1:]...)
		}
	}
	return appointments
}

func distinctNormalizedDOBs(appointments []models.Appointment) []string {
	seen := make(map[string]struct{})
	out := make([]string, 0, len(appointments))
	for _, appointment := range appointments {
		dob := normalizeDateKey(appointment.DOB)
		if dob == "" {
			continue
		}
		if _, ok := seen[dob]; ok {
			continue
		}
		seen[dob] = struct{}{}
		out = append(out, dob)
	}
	return out
}

func normalizeDateKey(value string) string {
	v := strings.TrimSpace(value)
	if v == "" {
		return ""
	}

	layouts := []string{
		"2006-01-02",
		"2006/01/02",
		"2006.01.02",
		"01-02-2006",
		"01/02/2006",
		"01.02.2006",
		"1-2-2006",
		"1/2/2006",
		"1.2.2006",
		"20060102",
		"01022006",
	}
	for _, layout := range layouts {
		if parsed, err := time.Parse(layout, v); err == nil {
			return parsed.Format("20060102")
		}
	}

	replacer := strings.NewReplacer("-", "", "/", "", ".", "", " ", "")
	return replacer.Replace(v)
}

func firstNameKey(fullName string) string {
	parts := strings.Fields(strings.ToLower(strings.TrimSpace(fullName)))
	if len(parts) == 0 {
		return ""
	}
	return parts[0]
}

func lastNameKey(fullName string) string {
	parts := strings.Fields(strings.ToLower(strings.TrimSpace(fullName)))
	if len(parts) == 0 {
		return ""
	}
	if len(parts) == 1 {
		return parts[0]
	}
	return strings.Join(parts[1:], " ")
}

func partialNameMatch(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	left := collapseName(a)
	right := collapseName(b)
	if left == "" || right == "" {
		return false
	}
	return left == right ||
		strings.HasPrefix(left, right) ||
		strings.HasPrefix(right, left) ||
		strings.Contains(left, right) ||
		strings.Contains(right, left)
}

func collapseName(value string) string {
	replacer := strings.NewReplacer(" ", "", "-", "", "'", "")
	return replacer.Replace(strings.ToLower(strings.TrimSpace(value)))
}

// writeEligibilityResult writes the eligibility JSON to disk.
// Path: artifacts/<officeKey>/<runStamp>/<payerSlug>/<patNum>_<aptNum>_eligibility.json
// TODO: also POST to the control-plane upload endpoint once that API is ready.
func writeEligibilityResult(appointment models.Appointment, el *eligibility.PatientEligibility, input payers.SessionInput, runStamp string) {
	dir := filepath.Join(
		"artifacts",
		sanitizeSegment(input.RequestedOfficeKey),
		runStamp,
		sanitizeSegment(input.Payer.PayerURL),
	)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		log.Printf("[DentaQuest] failed to create artifact dir %s: %v", dir, err)
		logging.Error("dentaquest", "dentaquest.artifact.mkdir_failed", "failed to create artifact directory", map[string]any{
			"dir":   dir,
			"error": err.Error(),
		})
		return
	}

	filename := fmt.Sprintf("%s_%s_eligibility.json",
		sanitizeSegment(appointment.PatNum),
		sanitizeSegment(appointment.AptNum),
	)
	filePath := filepath.Join(dir, filename)

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
		log.Printf("[DentaQuest] failed to marshal eligibility for patNum=%s: %v", appointment.PatNum, err)
		logging.Error("dentaquest", "dentaquest.artifact.marshal_failed", "failed to marshal eligibility artifact", map[string]any{
			"patNum": appointment.PatNum,
			"aptNum": appointment.AptNum,
			"error":  err.Error(),
		})
		return
	}

	if err := os.WriteFile(filePath, data, 0o644); err != nil {
		log.Printf("[DentaQuest] failed to write artifact %s: %v", filePath, err)
		logging.Error("dentaquest", "dentaquest.artifact.write_failed", "failed to write eligibility artifact", map[string]any{
			"patNum":   appointment.PatNum,
			"aptNum":   appointment.AptNum,
			"filePath": filePath,
			"error":    err.Error(),
		})
		return
	}

	logging.Info("dentaquest", "dentaquest.artifact.written", "eligibility artifact written", map[string]any{
		"patNum":   appointment.PatNum,
		"aptNum":   appointment.AptNum,
		"filePath": filePath,
	})
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

func writeResultsFile(dir, filename string, payload any) {
	data, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		log.Printf("[DentaQuest] marshal results file %s: %v", filename, err)
		return
	}
	if err := os.WriteFile(filepath.Join(dir, filename), data, 0o644); err != nil {
		log.Printf("[DentaQuest] write results file %s: %v", filename, err)
	}
}

var reUnsafe = regexp.MustCompile(`[<>:"/\\|?*\s]+`)

func sanitizeSegment(value string) string {
	return strings.Trim(reUnsafe.ReplaceAllString(value, "-"), "-")
}
