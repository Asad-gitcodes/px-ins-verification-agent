package uhcdental

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"insurance-benefit-agent-go/internal/advanced"
	"insurance-benefit-agent-go/internal/controlplane"
	"insurance-benefit-agent-go/internal/eligibility"
	"insurance-benefit-agent-go/internal/models"
	"insurance-benefit-agent-go/internal/payers"
	uhcapi "insurance-benefit-agent-go/internal/payers/uhcdental/api"
	uhcbrowser "insurance-benefit-agent-go/internal/payers/uhcdental/browser"
	uhceligibility "insurance-benefit-agent-go/internal/payers/uhcdental/eligibility"
	"insurance-benefit-agent-go/internal/resultwriter"
)

// PayerURL is the payer identifier as sent by the patcon server.
// Confirm the exact string from the patcon PayerURL field for UHC Dental.
const PayerURL = "UHCDental.com"

const loginCooldown = 45 * time.Second
const patientProbeGap = 5 * time.Second
const eligibilitySummarySettleDelay = 5 * time.Second

var (
	loginCooldownMu        sync.Mutex
	lastBrowserSessionStop time.Time
)

type appointmentTask struct {
	appointment models.Appointment
	tpCodes     []string
	spoolPath   string
	report      *advanced.PatientEligibilityReport
}

type probeSpool struct {
	PayerURL           string                             `json:"payerUrl"`
	Appointment        models.Appointment                 `json:"appointment"`
	MemberInfo         *uhcapi.MemberInfo                 `json:"memberInfo,omitempty"`
	BenefitSummary     *uhcapi.BenefitSummaryResponse     `json:"benefitSummary,omitempty"`
	UtilizationHistory *uhcapi.UtilizationHistoryResponse `json:"utilizationHistory,omitempty"`
	Notes              []string                           `json:"notes,omitempty"`
}

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
	var summary payers.RunSummary
	if !a.Supports(input.Payer.PayerURL) {
		return summary, fmt.Errorf("UHC Dental adapter does not support payerUrl=%s", input.Payer.PayerURL)
	}
	if input.SkipProbing {
		return a.runPhase2Only(ctx, input)
	}
	if len(input.Appointments) == 0 {
		return summary, fmt.Errorf("UHC Dental session requires at least one appointment")
	}

	runStamp := time.Now().UTC().Format("2006-01-02T15-04-05Z")
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
	// ── Phase 1: browser login → extract session cookies → close browser ──────
	if err := waitForLoginCooldown(ctx); err != nil {
		return summary, err
	}
	log.Printf("[UHCDental] launching browser for login")
	session, err := uhcbrowser.Launch(input)
	if err != nil {
		return summary, fmt.Errorf("UHC Dental browser login: %w", err)
	}
	browserClosed := false
	closeBrowser := func() {
		if browserClosed {
			return
		}
		browserClosed = true
		markBrowserSessionStopped()
		if closeErr := session.Close(); closeErr != nil {
			log.Printf("[UHCDental] browser close failed: %v", closeErr)
		}
	}
	defer closeBrowser()
	_, cookieErr := session.ExtractSessionCookies()
	if cookieErr != nil {
		return summary, fmt.Errorf("UHC Dental extract session cookies: %w", cookieErr)
	}
	if err := os.MkdirAll(tempProbeDir, 0o755); err != nil {
		return summary, fmt.Errorf("create UHC temp probe dir: %w", err)
	}
	log.Printf("[UHCDental] keeping temp probe files in %s", tempProbeDir)

	// ── Phase 2: API probe — one member search + 2 data calls per patient ─────
	var tasks []appointmentTask

	for i, appointment := range input.Appointments {
		select {
		case <-ctx.Done():
			return summary, ctx.Err()
		default:
		}

		log.Printf("[UHCDental] processing patNum=%s aptNum=%s", appointment.PatNum, appointment.AptNum)

		task, spool, err := processAppointment(ctx, session, appointment)
		if err != nil {
			log.Printf("[UHCDental] appointment failed patNum=%s aptNum=%s: %v",
				appointment.PatNum, appointment.AptNum, err)
			if spool != nil {
				spool.Notes = append(spool.Notes, "probe failed: "+err.Error())
			} else {
				writeProbeError(tempProbeDir, appointment, err)
				task = appointmentTask{
					appointment: appointment,
					report:      payers.BuildNotFoundReport(appointment),
				}
			}
		}

		if spool != nil {
			spoolPath, spoolErr := writeProbeSpool(tempProbeDir, appointment, spool)
			if spoolErr != nil {
				log.Printf("[UHCDental] temp probe write failed patNum=%s aptNum=%s: %v",
					appointment.PatNum, appointment.AptNum, spoolErr)
				task = appointmentTask{
					appointment: appointment,
					report:      payers.BuildUnableToDetermineReport(appointment),
				}
			} else {
				task.spoolPath = spoolPath
				logUHCProbeSummary(appointment, spool, spoolPath)
			}
		}

		tasks = append(tasks, task)

		if i < len(input.Appointments)-1 {
			if err := sleepWithContext(ctx, patientProbeGap); err != nil {
				return summary, err
			}
		}
	}

	closeBrowser()
	log.Printf("[UHCDental] phase 2 paused; raw probe files kept in %s", tempProbeDir)
	return summary, nil

	writer, writerErr := resultwriter.New(input.Testing, input.ScraperConfig.APIs)
	if writerErr != nil {
		log.Printf("[UHCDental] resultwriter unavailable - apptField/PDF upload disabled: %v", writerErr)
	}

	for i := range tasks {
		task := &tasks[i]
		if task.report == nil && task.spoolPath != "" {
			spool, readErr := readProbeSpool(task.spoolPath)
			if readErr != nil {
				log.Printf("[UHCDental] temp probe read failed patNum=%s aptNum=%s: %v",
					task.appointment.PatNum, task.appointment.AptNum, readErr)
				task.report = payers.BuildUnableToDetermineReport(task.appointment)
			} else {
				el := buildEligibilityFromSpool(spool)
				if el == nil {
					if spool.MemberInfo != nil && !spool.MemberInfo.IsEligible() {
						r := payers.BuildNotActiveReport(task.appointment, "", "UnitedHealthCare Dental", "")
						r.Patient.FullName = spool.MemberInfo.FullName()
						task.report = r
					} else {
						task.report = payers.BuildUnableToDetermineReport(task.appointment)
					}
				} else {
					task.report = advanced.Build(el, input.OfficeCodes, task.tpCodes)
					if task.report == nil {
						task.report = payers.BuildUnableToDetermineReport(task.appointment)
					}
					if input.Testing.ShouldWriteDebugArtifacts() {
						writeEligibilityResult(outputDir, task.appointment, el, input)
					}
				}
				if input.Testing.ShouldWriteDebugArtifacts() {
					writeAdvancedResult(outputDir, task.appointment, task.report)
				}
			}
		}

		status := apptStatus(task.report)
		summary.RecordAppointment(task.appointment, status)
		log.Printf("[UHCDental] finalizing result patNum=%s aptNum=%s status=%s",
			task.appointment.PatNum, task.appointment.AptNum, status)
		if writer != nil {
			writer.ApplyResult(task.appointment, status, input.RequestedOfficeKey, nil, false)
		}
		input.QueuePDFTask(task.appointment, status, task.report, outputDir)
	}

	return summary, nil
}

func scanProbeSpoolAppointments(probeDir, payerURL string) []models.Appointment {
	prefix := payers.SanitizeProbeSegment(payerURL) + "_"
	matches, _ := filepath.Glob(filepath.Join(probeDir, prefix+"*_probe.json"))
	var result []models.Appointment
	for _, f := range matches {
		spool, err := readProbeSpool(f)
		if err != nil || spool == nil {
			continue
		}
		result = append(result, spool.Appointment)
	}
	return result
}

func (a *Adapter) runPhase2Only(ctx context.Context, input payers.SessionInput) (payers.RunSummary, error) {
	var summary payers.RunSummary
	outputDir := filepath.Join("artifacts", "results")
	if err := os.MkdirAll(outputDir, 0o755); err != nil {
		return summary, fmt.Errorf("create UHC Dental results dir: %w", err)
	}

	appointments := input.Appointments
	if len(appointments) == 0 {
		appointments = scanProbeSpoolAppointments(input.ProbeOutputDir, PayerURL)
		log.Printf("[UHCDental] skipProbing bucket scan: found %d probe files in %s", len(appointments), input.ProbeOutputDir)
	}
	if len(appointments) == 0 {
		log.Printf("[UHCDental] skipProbing: no probe files found, nothing to postprocess")
		return summary, nil
	}
	log.Printf("[UHCDental] skipProbing=true reading probes from %s", input.ProbeOutputDir)

	writer, writerErr := resultwriter.New(input.Testing, input.ScraperConfig.APIs)
	if writerErr != nil {
		log.Printf("[UHCDental] resultwriter unavailable: %v", writerErr)
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

		probePath := payers.ProbeFilePathForAppointment(input.ProbeOutputDir, PayerURL, appointment, "probe")
		spool, readErr := readProbeSpool(probePath)
		if readErr != nil {
			log.Printf("[UHCDental] skipProbing read failed patNum=%s aptNum=%s: %v", appointment.PatNum, appointment.AptNum, readErr)
			if probeErr, err := payers.ReadProbeErrorForAppointment(input.ProbeOutputDir, PayerURL, appointment); err == nil {
				statusOverride = resultwriter.StatusForProbeErrorType(probeErr.ErrorType)
				log.Printf("[UHCDental] probe error result patNum=%s aptNum=%s errorType=%s error=%q", appointment.PatNum, appointment.AptNum, probeErr.ErrorType, probeErr.Error)
			}
			report = payers.BuildUnableToDetermineReport(appointment)
		} else {
			el := buildEligibilityFromSpool(spool)
			if el == nil {
				if spool.MemberInfo != nil && !spool.MemberInfo.IsEligible() {
					r := payers.BuildNotActiveReport(appointment, "", "UnitedHealthCare Dental", "")
					r.Patient.FullName = spool.MemberInfo.FullName()
					report = r
				} else {
					report = payers.BuildUnableToDetermineReport(appointment)
				}
			} else {
				report = advanced.Build(el, input.OfficeCodes, tpCodes)
				if report == nil {
					report = payers.BuildUnableToDetermineReport(appointment)
				}
				writeEligibilityResult(outputDir, appointment, el, input)
			}
			writeAdvancedResult(outputDir, appointment, report)
		}

		status := apptStatus(report)
		if statusOverride != "" {
			status = statusOverride
		}
		summary.RecordAppointment(appointment, status)
		log.Printf("[UHCDental] skipProbing result patNum=%s aptNum=%s status=%s", appointment.PatNum, appointment.AptNum, status)
		if writer != nil {
			writer.ApplyResult(appointment, status, input.RequestedOfficeKey, nil, false)
		}
		input.QueuePDFTask(appointment, status, report, outputDir)
	}

	return summary, nil
}

func logUHCProbeSummary(appointment models.Appointment, spool *probeSpool, spoolPath string) {
	if spool == nil {
		return
	}
	member := ""
	eligible := false
	planID := ""
	if spool.MemberInfo != nil {
		member = spool.MemberInfo.FullName()
		eligible = spool.MemberInfo.IsEligible()
		planID = spool.MemberInfo.PlanID
	}
	planBenefits := 0
	categories := 0
	if spool.BenefitSummary != nil {
		planBenefits = len(spool.BenefitSummary.Result.DentalBenefitsAndAccums.PlanLevelBenefits)
		categories = len(spool.BenefitSummary.Result.DentalBenefitsAndAccums.CategoryLevelBenefits)
	}
	procedures := 0
	if spool.UtilizationHistory != nil {
		procedures = len(spool.UtilizationHistory.Result.DentalServiceHistory.Procedures)
	}
	log.Printf("[UHCDental] probe summary patNum=%s aptNum=%s eligible=%t member=%q planId=%q planBenefits=%d categories=%d procedures=%d notes=%d file=%s",
		appointment.PatNum, appointment.AptNum, eligible, member, planID, planBenefits, categories, procedures, len(spool.Notes), spoolPath)
}

func processAppointment(
	ctx context.Context,
	session *uhcbrowser.Session,
	appointment models.Appointment,
) (appointmentTask, *probeSpool, error) {
	task := appointmentTask{appointment: appointment}
	if appointment.TreatmentPlanProcCodes != "" {
		task.tpCodes = strings.Split(appointment.TreatmentPlanProcCodes, ",")
	}
	spool := &probeSpool{
		PayerURL:    PayerURL,
		Appointment: appointment,
	}

	subscriberID := appointment.SubscriberID
	if subscriberID == "" {
		return task, nil, fmt.Errorf("appointment has no subscriberID")
	}

	// UHC's search DOB is the patient/member DOB, not the subscriber DOB.
	dob := appointment.DOB
	if dob == "" {
		dob = appointment.SubDOB
	}
	dob = normalizeDOB(dob)
	serviceDate := normalizeDate(appointment.AppointmentDate)

	// Step 1: drive the visible search form and land on eligibility-summary.
	mi, err := session.SearchMemberViaGUI(subscriberID, dob, serviceDate)
	if err != nil {
		log.Printf("[UHCDental] api memberSearch patNum=%s aptNum=%s status=error err=%v",
			appointment.PatNum, appointment.AptNum, err)
		return task, nil, fmt.Errorf("member search: %w", err)
	}
	spool.MemberInfo = mi
	log.Printf("[UHCDental] api memberSearch patNum=%s aptNum=%s status=ok eligible=%t member=%q",
		appointment.PatNum, appointment.AptNum, mi.IsEligible(), mi.FullName())

	if err := sleepWithContext(ctx, eligibilitySummarySettleDelay); err != nil {
		return task, spool, err
	}

	planID, planErr := session.WaitForPlanID()
	if planErr != nil {
		log.Printf("[UHCDental] api planId patNum=%s aptNum=%s status=error err=%v",
			appointment.PatNum, appointment.AptNum, planErr)
		return task, spool, fmt.Errorf("planId lookup: %w", planErr)
	}
	mi.PlanID = strings.TrimSpace(planID)
	log.Printf("[UHCDental] api planId patNum=%s aptNum=%s status=ok planId=%q",
		appointment.PatNum, appointment.AptNum, mi.PlanID)

	if !mi.IsEligible() {
		log.Printf("[UHCDental] stored inactive member patNum=%s aptNum=%s member=%s",
			appointment.PatNum, appointment.AptNum, mi.FullName())
		if err := session.ResetToSearchLanding(); err != nil {
			log.Printf("[UHCDental] reset to search-landing failed patNum=%s aptNum=%s: %v",
				appointment.PatNum, appointment.AptNum, err)
		}
		return task, spool, nil
	}

	// Step 2: refresh cookies after eligibility-summary settles; downstream API
	// calls depend on the browser-updated member and plan session state.
	refreshedCookies, err := session.ExtractSessionCookies()
	if err != nil {
		return task, spool, fmt.Errorf("refresh session cookies after eligibility-summary: %w", err)
	}
	probe, err := uhcapi.NewProbe(refreshedCookies)
	if err != nil {
		return task, spool, fmt.Errorf("UHC Dental create refreshed probe: %w", err)
	}

	// Step 3: benefit summary
	bs, err := probe.FetchBenefitSummary(ctx, mi.PlanID, mi.MemberContrivedKey, serviceDate)
	if err != nil {
		log.Printf("[UHCDental] api benefitSummary patNum=%s aptNum=%s status=error err=%v",
			appointment.PatNum, appointment.AptNum, err)
		return task, spool, fmt.Errorf("benefitsummary: %w", err)
	}
	spool.BenefitSummary = bs
	log.Printf("[UHCDental] api benefitSummary patNum=%s aptNum=%s status=ok planBenefits=%d categories=%d",
		appointment.PatNum, appointment.AptNum,
		len(bs.Result.DentalBenefitsAndAccums.PlanLevelBenefits),
		len(bs.Result.DentalBenefitsAndAccums.CategoryLevelBenefits))

	// Step 4: utilization history — non-fatal: UHC returns 500 when the member has
	// no service history. Continue with an empty response so benefitSummary data
	// is still captured in the eligibility result.
	uh, err := probe.FetchUtilizationHistory(ctx, mi.MemberContrivedKey, mi.PlanID)
	if err != nil {
		log.Printf("[UHCDental] api utilizationHistory patNum=%s aptNum=%s status=error err=%v (continuing with empty history)",
			appointment.PatNum, appointment.AptNum, err)
		spool.Notes = append(spool.Notes, "utilizationHistory unavailable: "+err.Error())
		uh = &uhcapi.UtilizationHistoryResponse{}
	} else {
		log.Printf("[UHCDental] api utilizationHistory patNum=%s aptNum=%s status=ok procedures=%d",
			appointment.PatNum, appointment.AptNum,
			len(uh.Result.DentalServiceHistory.Procedures))
	}
	spool.UtilizationHistory = uh

	if err := session.ResetToSearchLanding(); err != nil {
		log.Printf("[UHCDental] reset to search-landing failed patNum=%s aptNum=%s: %v",
			appointment.PatNum, appointment.AptNum, err)
	}
	return task, spool, nil
}

// ── helpers ───────────────────────────────────────────────────────────────────

func apptStatus(report *advanced.PatientEligibilityReport) string {
	if report == nil || report.StatusOnly {
		return resultwriter.ApptStatusNotFound
	}
	if !report.Patient.IsEligible {
		return resultwriter.ApptStatusInactive
	}
	return resultwriter.ApptStatusVerified
}

func writeAdvancedResult(outputDir string, appointment models.Appointment, report *advanced.PatientEligibilityReport) {
	if report == nil {
		return
	}
	path := filepath.Join(outputDir, fmt.Sprintf("%s_%s_advanced.json",
		sanitizeSegment(appointment.PatNum), sanitizeSegment(appointment.AptNum)))
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return
	}
	_ = os.WriteFile(path, data, 0o644)
}

func writeProbeSpool(tempProbeDir string, appointment models.Appointment, spool *probeSpool) (string, error) {
	if spool == nil {
		return "", fmt.Errorf("probe spool is nil")
	}
	path := payers.ProbeFilePathForAppointment(tempProbeDir, PayerURL, appointment, "probe")
	data, err := json.MarshalIndent(spool, "", "  ")
	if err != nil {
		return "", fmt.Errorf("marshal probe spool: %w", err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return "", fmt.Errorf("write probe spool: %w", err)
	}
	return path, nil
}

func writeProbeError(tempProbeDir string, appointment models.Appointment, probeErr error) {
	path := payers.ProbeFilePathForAppointment(tempProbeDir, PayerURL, appointment, "probe_error")
	payload := map[string]any{
		"recordedAt":  time.Now().UTC().Format(time.RFC3339),
		"appointment": appointment,
		"error":       probeErr.Error(),
		"errorType":   payers.ClassifyProbeError(probeErr),
	}
	data, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		log.Printf("[UHCDental] marshal probe error failed patNum=%s aptNum=%s: %v",
			appointment.PatNum, appointment.AptNum, err)
		return
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		log.Printf("[UHCDental] write probe error failed patNum=%s aptNum=%s: %v",
			appointment.PatNum, appointment.AptNum, err)
	}
}

func readProbeSpool(path string) (*probeSpool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read probe spool: %w", err)
	}
	var spool probeSpool
	if err := json.Unmarshal(data, &spool); err != nil {
		return nil, fmt.Errorf("unmarshal probe spool: %w", err)
	}
	return &spool, nil
}

func buildEligibilityFromSpool(spool *probeSpool) *eligibility.PatientEligibility {
	if spool == nil || spool.MemberInfo == nil || spool.BenefitSummary == nil {
		return nil
	}
	uh := spool.UtilizationHistory
	if uh == nil {
		uh = &uhcapi.UtilizationHistoryResponse{}
	}
	return uhceligibility.Build(spool.BenefitSummary, uh, spool.MemberInfo)
}

func writeEligibilityResult(outputDir string, appointment models.Appointment, el *eligibility.PatientEligibility, input payers.SessionInput) {
	if el == nil {
		return
	}
	path := filepath.Join(outputDir, fmt.Sprintf("%s_%s_eligibility.json",
		sanitizeSegment(appointment.PatNum), sanitizeSegment(appointment.AptNum)))
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
		return
	}
	_ = os.WriteFile(path, data, 0o644)
}

func waitForLoginCooldown(ctx context.Context) error {
	loginCooldownMu.Lock()
	defer loginCooldownMu.Unlock()

	if lastBrowserSessionStop.IsZero() {
		return nil
	}

	waitFor := time.Until(lastBrowserSessionStop.Add(loginCooldown))
	if waitFor <= 0 {
		return nil
	}

	log.Printf("[UHCDental] cooling down before next login for %s", waitFor.Round(time.Second))
	timer := time.NewTimer(waitFor)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func markBrowserSessionStopped() {
	loginCooldownMu.Lock()
	defer loginCooldownMu.Unlock()
	lastBrowserSessionStop = time.Now()
}

func sleepWithContext(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// normalizeDate converts any common date format to MM/DD/YYYY for UHC's API.
// normalizeISODate converts MM/DD/YYYY to YYYY-MM-DD for APIs that need ISO dates.
func normalizeISODate(d string) string {
	if t, err := time.Parse("01/02/2006", d); err == nil {
		return t.Format("2006-01-02")
	}
	return d
}

func normalizeDate(d string) string {
	inputs := []string{"2006-01-02", "01-02-2006", "01/02/2006", "1/2/2006"}
	for _, layout := range inputs {
		if t, err := time.Parse(layout, d); err == nil {
			return t.Format("01/02/2006")
		}
	}
	return d
}

// normalizeDOB converts any common DOB format to MM/DD/YYYY for UHC's API.
// Handles: 2006-01-02, 01-02-2006, 01/02/2006.
func normalizeDOB(dob string) string {
	inputs := []string{"2006-01-02", "01-02-2006", "01/02/2006", "1/2/2006"}
	for _, layout := range inputs {
		if t, err := time.Parse(layout, dob); err == nil {
			return t.Format("01/02/2006")
		}
	}
	return dob
}

func sanitizeSegment(s string) string {
	return strings.NewReplacer("/", "_", "\\", "_", ":", "_", " ", "_").Replace(s)
}
