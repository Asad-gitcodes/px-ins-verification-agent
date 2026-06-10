package metlife

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"insurance-benefit-agent-go/internal/advanced"
	"insurance-benefit-agent-go/internal/controlplane"
	"insurance-benefit-agent-go/internal/eligibility"
	"insurance-benefit-agent-go/internal/models"
	"insurance-benefit-agent-go/internal/payers"
	metlifeapi "insurance-benefit-agent-go/internal/payers/metlife/api"
	metlifebrowser "insurance-benefit-agent-go/internal/payers/metlife/browser"
	metlifeeligibility "insurance-benefit-agent-go/internal/payers/metlife/eligibility"
	"insurance-benefit-agent-go/internal/resultwriter"
)

const (
	PayerURL             = "metlife.com"
	defaultPlanTypeCode  = "202"
	planOverviewPPOInd   = "Y"
	procedureCodesPPOInd = "0"
)

type Adapter struct {
	control *controlplane.Client
}

type probeSpool struct {
	PayerURL                   string                                        `json:"payerUrl"`
	Appointment                models.Appointment                            `json:"appointment"`
	OverviewRequest            metlifeapi.EligibilityOverviewRequest         `json:"overviewRequest"`
	Overview                   *metlifeapi.EligibilityOverviewResponse       `json:"overview,omitempty"`
	CoveredPerson              *metlifeapi.CoveredPerson                     `json:"coveredPerson,omitempty"`
	PlanOverviewRequest        *metlifeapi.PlanOverviewRequest               `json:"planOverviewRequest,omitempty"`
	PlanOverview               *metlifeapi.PlanOverviewResponse              `json:"planOverview,omitempty"`
	ProcedureCategoriesRequest *metlifeapi.ProcedureCategoriesRequest        `json:"procedureCategoriesRequest,omitempty"`
	ProcedureCategories        *metlifeapi.ProcedureCategoriesResponse       `json:"procedureCategories,omitempty"`
	ProvidersRequest           *metlifeapi.ProvidersRequest                  `json:"providersRequest,omitempty"`
	Providers                  *metlifeapi.ProvidersResponse                 `json:"providers,omitempty"`
	SelectedProvider           *metlifeapi.Provider                          `json:"selectedProvider,omitempty"`
	ProcedureCodeRequests      []metlifeapi.ProcedureCodesRequest            `json:"procedureCodeRequests,omitempty"`
	ProcedureCodeResponses     map[string]*metlifeapi.ProcedureCodesResponse `json:"procedureCodeResponses,omitempty"`
	Notes                      []string                                      `json:"notes,omitempty"`
}

type appointmentTask struct {
	appointment models.Appointment
	tpCodes     []string
	spoolPath   string
	report      *advanced.PatientEligibilityReport
}

func NewAdapter(control *controlplane.Client) *Adapter {
	return &Adapter{control: control}
}

func (a *Adapter) PayerURL() string { return PayerURL }

func (a *Adapter) Supports(payerURL string) bool {
	return strings.EqualFold(payerURL, PayerURL)
}

func (a *Adapter) Run(ctx context.Context, input payers.SessionInput) (payers.RunSummary, error) {
	_ = a.control
	var summary payers.RunSummary
	if !a.Supports(input.Payer.PayerURL) {
		return summary, fmt.Errorf("MetLife adapter does not support payerUrl=%s", input.Payer.PayerURL)
	}
	if input.SkipProbing {
		return a.runPhase2Only(ctx, input)
	}
	if len(input.Appointments) == 0 {
		return summary, fmt.Errorf("MetLife session requires at least one appointment")
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
	if err := os.MkdirAll(tempProbeDir, 0o755); err != nil {
		return summary, fmt.Errorf("create MetLife temp probe dir: %w", err)
	}
	log.Printf("[MetLife] keeping temp probe files in %s", tempProbeDir)

	log.Printf("[MetLife] launching browser for login")
	session, err := metlifebrowser.Launch(input)
	if err != nil {
		return summary, fmt.Errorf("MetLife browser launch: %w", err)
	}
	log.Printf("[MetLife] browser ready, starting login")
	browserClosed := false
	closeBrowser := func() {
		if browserClosed {
			return
		}
		browserClosed = true
		if closeErr := session.Close(); closeErr != nil {
			log.Printf("[MetLife] browser close failed: %v", closeErr)
		}
	}
	defer closeBrowser()

	if err := session.Login(input); err != nil {
		return summary, fmt.Errorf("MetLife login: %w", err)
	}

	probe, err := metlifeapi.NewProbe(session.Page())
	if err != nil {
		return summary, fmt.Errorf("MetLife create probe: %w", err)
	}

	var tasks []appointmentTask

	// Phase 1: scrape all appointments while the headed browser is open.
	for i, appointment := range input.Appointments {
		select {
		case <-ctx.Done():
			return summary, ctx.Err()
		default:
		}

		if i > 0 {
			// Brief pause between patients so back-to-back XHR fetches don't
			// trigger Akamai's behavioral bot detection and invalidate the session.
			time.Sleep(2 * time.Second)
		}

		log.Printf("[MetLife] processing patNum=%s aptNum=%s subscriberId=%s",
			appointment.PatNum, appointment.AptNum, appointment.SubscriberID)

		var tpCodes []string
		if appointment.TreatmentPlanProcCodes != "" {
			tpCodes = strings.Split(appointment.TreatmentPlanProcCodes, ",")
		}

		task := appointmentTask{
			appointment: appointment,
			tpCodes:     tpCodes,
		}
		spool, probeErr := processAppointment(ctx, probe, input, appointment)
		if probeErr != nil {
			log.Printf("[MetLife] probe failed patNum=%s aptNum=%s: %v", appointment.PatNum, appointment.AptNum, probeErr)
			if spool != nil {
				spool.Notes = append(spool.Notes, "probe failed: "+probeErr.Error())
			} else {
				writeProbeError(tempProbeDir, appointment, probeErr)
				task.report = payers.BuildUnableToDetermineReport(appointment)
			}
		}

		if spool != nil {
			spoolPath, spoolErr := writeProbeSpool(tempProbeDir, appointment, spool)
			if spoolErr != nil {
				log.Printf("[MetLife] temp probe write failed patNum=%s aptNum=%s: %v",
					appointment.PatNum, appointment.AptNum, spoolErr)
				task.report = payers.BuildUnableToDetermineReport(appointment)
			} else {
				task.spoolPath = spoolPath
				logMetLifeProbeSummary(appointment, spool, spoolPath)
			}
		}

		tasks = append(tasks, task)
	}

	closeBrowser()
	log.Printf("[MetLife] phase 2 paused; raw probe files kept in %s", tempProbeDir)
	return summary, nil

	writer, writerErr := resultwriter.New(input.Testing, input.ScraperConfig.APIs)
	if writerErr != nil {
		log.Printf("[MetLife] resultwriter unavailable - apptField/PDF upload disabled: %v", writerErr)
	}

	for i := range tasks {
		task := &tasks[i]
		if task.report == nil && task.spoolPath != "" {
			spool, readErr := readProbeSpool(task.spoolPath)
			if readErr != nil {
				log.Printf("[MetLife] temp probe read failed patNum=%s aptNum=%s: %v",
					task.appointment.PatNum, task.appointment.AptNum, readErr)
				task.report = payers.BuildUnableToDetermineReport(task.appointment)
			} else {
				el := buildEligibilityFromSpool(spool)
				task.report = buildReportFromSpool(spool, el, input.OfficeCodes, task.tpCodes)
				if input.Testing.ShouldWriteDebugArtifacts() {
					writeProbeResult(outputDir, task.appointment, spool)
					writeEligibilityResult(outputDir, task.appointment, el, input)
					writeAdvancedResult(outputDir, task.appointment, task.report)
				}
			}
		}

		status := apptStatus(task.report)
		summary.RecordAppointment(task.appointment, status)
		log.Printf("[MetLife] finalizing result patNum=%s aptNum=%s status=%s",
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
		return summary, fmt.Errorf("create MetLife results dir: %w", err)
	}

	appointments := input.Appointments
	if len(appointments) == 0 {
		appointments = scanProbeSpoolAppointments(input.ProbeOutputDir, PayerURL)
		log.Printf("[MetLife] skipProbing bucket scan: found %d probe files in %s", len(appointments), input.ProbeOutputDir)
	}
	if len(appointments) == 0 {
		log.Printf("[MetLife] skipProbing: no probe files found, nothing to postprocess")
		return summary, nil
	}
	log.Printf("[MetLife] skipProbing=true reading probes from %s", input.ProbeOutputDir)

	writer, writerErr := resultwriter.New(input.Testing, input.ScraperConfig.APIs)
	if writerErr != nil {
		log.Printf("[MetLife] resultwriter unavailable: %v", writerErr)
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
			log.Printf("[MetLife] skipProbing read failed patNum=%s aptNum=%s: %v", appointment.PatNum, appointment.AptNum, readErr)
			if probeErr, err := payers.ReadProbeErrorForAppointment(input.ProbeOutputDir, PayerURL, appointment); err == nil {
				statusOverride = resultwriter.StatusForProbeErrorType(probeErr.ErrorType)
				log.Printf("[MetLife] probe error result patNum=%s aptNum=%s errorType=%s error=%q", appointment.PatNum, appointment.AptNum, probeErr.ErrorType, probeErr.Error)
			}
			report = payers.BuildUnableToDetermineReport(appointment)
		} else {
			el := buildEligibilityFromSpool(spool)
			report = buildReportFromSpool(spool, el, input.OfficeCodes, tpCodes)
			if report == nil {
				report = payers.BuildUnableToDetermineReport(appointment)
			}
			writeEligibilityResult(outputDir, appointment, el, input)
			writeAdvancedResult(outputDir, appointment, report)
		}

		status := apptStatus(report)
		if statusOverride != "" {
			status = statusOverride
		}
		summary.RecordAppointment(appointment, status)
		log.Printf("[MetLife] skipProbing result patNum=%s aptNum=%s status=%s", appointment.PatNum, appointment.AptNum, status)
		if writer != nil {
			writer.ApplyResult(appointment, status, input.RequestedOfficeKey, nil, false)
		}
		input.QueuePDFTask(appointment, status, report, outputDir)
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

func logMetLifeProbeSummary(appointment models.Appointment, spool *probeSpool, spoolPath string) {
	if spool == nil {
		return
	}
	member := ""
	coverage := ""
	if spool.CoveredPerson != nil {
		member = strings.TrimSpace(strings.Join([]string{spool.CoveredPerson.FirstName, spool.CoveredPerson.LastName}, " "))
		coverage = spool.CoveredPerson.CoverageStatus
	}
	coveredPersons := 0
	if spool.Overview != nil {
		coveredPersons = len(spool.Overview.CoveredPersons)
	}
	providers := 0
	if spool.Providers != nil {
		providers = len(spool.Providers.Providers)
	}
	codeResponses := 0
	codeProcedures := 0
	for _, resp := range spool.ProcedureCodeResponses {
		if resp == nil {
			continue
		}
		codeResponses++
		codeProcedures += len(resp.Procedures)
	}
	log.Printf("[MetLife] probe summary patNum=%s aptNum=%s member=%q coverage=%q coveredPersons=%d categories=%d providers=%d codeResponses=%d codeProcedures=%d notes=%d file=%s",
		appointment.PatNum, appointment.AptNum, member, coverage, coveredPersons,
		countMetLifeCategories(spool.ProcedureCategories), providers, codeResponses, codeProcedures, len(spool.Notes), spoolPath)
}

func countMetLifeCategories(resp *metlifeapi.ProcedureCategoriesResponse) int {
	if resp == nil {
		return 0
	}
	total := 0
	for _, insured := range resp.Insureds {
		for _, group := range insured.ProcedureCategoryGroups {
			total += len(group.ProcedureCategories)
		}
	}
	return total
}

func metlifeProcedureCategoriesOK(resp *metlifeapi.ProcedureCategoriesResponse) bool {
	if resp == nil {
		return false
	}
	return metlifeOutcomeStatus(resp) == "ok" && countMetLifeCategories(resp) > 0
}

func metlifeOutcomeStatus(resp *metlifeapi.ProcedureCategoriesResponse) string {
	if resp == nil {
		return "nil"
	}
	outcome := resp.MetaData.Outcome
	if outcome.StatusCode >= 200 && outcome.StatusCode < 300 && strings.EqualFold(outcome.Message, "SUCCESS") {
		return "ok"
	}
	if outcome.StatusCode != 0 || outcome.Message != "" {
		return fmt.Sprintf("error(%d:%s)", outcome.StatusCode, outcome.Message)
	}
	return "unknown"
}

func metlifeEmployeeIDFallbacks(person *metlifeapi.CoveredPerson, appointment models.Appointment, used string) []string {
	candidates := []string{
		strings.TrimSpace(person.EmployeeID),
		strings.TrimSpace(person.ActualID),
		strings.TrimSpace(appointment.SubscriberID),
	}
	seen := map[string]struct{}{strings.TrimSpace(used): struct{}{}}
	out := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		if candidate == "" {
			continue
		}
		if _, ok := seen[candidate]; ok {
			continue
		}
		seen[candidate] = struct{}{}
		out = append(out, candidate)
	}
	return out
}

func processAppointment(
	ctx context.Context,
	probe *metlifeapi.Probe,
	input payers.SessionInput,
	appointment models.Appointment,
) (*probeSpool, error) {
	employeeID := strings.TrimSpace(appointment.SubscriberID)
	if employeeID == "" {
		return nil, fmt.Errorf("appointment has no subscriberId")
	}

	spool := &probeSpool{
		PayerURL:               PayerURL,
		Appointment:            appointment,
		OverviewRequest:        buildOverviewRequest(appointment),
		ProcedureCodeResponses: make(map[string]*metlifeapi.ProcedureCodesResponse),
	}

	overview, err := probe.FetchEligibilityOverview(ctx, spool.OverviewRequest)
	if err != nil {
		log.Printf("[MetLife] api overview patNum=%s aptNum=%s status=error err=%v",
			appointment.PatNum, appointment.AptNum, err)
		return spool, fmt.Errorf("eligibility overview: %w", err)
	}
	spool.Overview = overview
	log.Printf("[MetLife] api overview patNum=%s aptNum=%s status=ok coveredPersons=%d reason=%q",
		appointment.PatNum, appointment.AptNum, len(overview.CoveredPersons), overview.ReasonMessage)
	if shouldRetryOverviewWithoutZip(overview, spool.OverviewRequest) {
		retryReq := spool.OverviewRequest
		retryReq.ZipCode = ""
		retryOverview, retryErr := probe.FetchEligibilityOverview(ctx, retryReq)
		if retryErr != nil {
			spool.Notes = append(spool.Notes, "overview retry without zip failed: "+retryErr.Error())
			log.Printf("[MetLife] api overviewRetryNoZip patNum=%s aptNum=%s status=error err=%v",
				appointment.PatNum, appointment.AptNum, retryErr)
		} else if len(retryOverview.CoveredPersons) > 0 {
			spool.Notes = append(spool.Notes, "overview retried without zip after zip mismatch")
			spool.OverviewRequest = retryReq
			spool.Overview = retryOverview
			overview = retryOverview
			log.Printf("[MetLife] api overviewRetryNoZip patNum=%s aptNum=%s status=ok coveredPersons=%d",
				appointment.PatNum, appointment.AptNum, len(retryOverview.CoveredPersons))
		}
	}

	coveredPerson := selectCoveredPerson(overview.CoveredPersons, appointment)
	if coveredPerson == nil {
		return spool, nil
	}
	spool.CoveredPerson = coveredPerson

	// Not active: build minimal eligibility and return early (skip expensive API calls).
	if !strings.EqualFold(strings.TrimSpace(coveredPerson.CoverageStatus), "active") {
		return spool, nil
	}
	memberEmployeeID := metlifeCoveredPersonEmployeeID(coveredPerson, appointment)

	planReq := &metlifeapi.PlanOverviewRequest{
		EmployeeID:              memberEmployeeID,
		GroupNumber:             coveredPerson.GroupNumber,
		CustomerNumber:          coveredPerson.CustomerNumber,
		Branch:                  coveredPerson.Branch,
		SubDivision:             coveredPerson.SubDivision,
		DependentSequenceNumber: coveredPerson.DependentSequenceNumber,
		RelationshipCode:        coveredPerson.RelationShipCode,
		PPOInd:                  planOverviewPPOInd,
	}
	spool.PlanOverviewRequest = planReq
	planOverview, err := probe.FetchPlanOverview(ctx, *planReq)
	if err != nil {
		log.Printf("[MetLife] api planOverview patNum=%s aptNum=%s status=error err=%v",
			appointment.PatNum, appointment.AptNum, err)
		return spool, fmt.Errorf("plan overview: %w", err)
	}
	spool.PlanOverview = planOverview
	log.Printf("[MetLife] api planOverview patNum=%s aptNum=%s status=ok provisions=%d",
		appointment.PatNum, appointment.AptNum, len(planOverview.PlanProvisions))

	categoriesEmployeeID := metlifeCoveredPersonEmployeeID(coveredPerson, appointment)
	categoriesReq := &metlifeapi.ProcedureCategoriesRequest{
		EmployeeID:     categoriesEmployeeID,
		CustomerNumber: coveredPerson.CustomerNumber,
		PlanTypeCode:   defaultPlanTypeCode,
	}
	spool.ProcedureCategoriesRequest = categoriesReq
	categories, err := probe.FetchProcedureCategories(ctx, *categoriesReq)
	if err != nil {
		log.Printf("[MetLife] api procedureCategories patNum=%s aptNum=%s status=error err=%v",
			appointment.PatNum, appointment.AptNum, err)
		return spool, fmt.Errorf("procedure categories: %w", err)
	}
	if !metlifeProcedureCategoriesOK(categories) {
		for _, fallbackEmployeeID := range metlifeEmployeeIDFallbacks(coveredPerson, appointment, categoriesEmployeeID) {
			retryReq := *categoriesReq
			retryReq.EmployeeID = fallbackEmployeeID
			retryCategories, retryErr := probe.FetchProcedureCategories(ctx, retryReq)
			if retryErr != nil {
				spool.Notes = append(spool.Notes, fmt.Sprintf("procedure categories retry employeeId=%s failed: %v", fallbackEmployeeID, retryErr))
				log.Printf("[MetLife] api procedureCategoriesRetry patNum=%s aptNum=%s status=error employeeId=%s err=%v",
					appointment.PatNum, appointment.AptNum, fallbackEmployeeID, retryErr)
				continue
			}
			log.Printf("[MetLife] api procedureCategoriesRetry patNum=%s aptNum=%s status=%s employeeId=%s insureds=%d categories=%d",
				appointment.PatNum, appointment.AptNum, metlifeOutcomeStatus(retryCategories), fallbackEmployeeID,
				len(retryCategories.Insureds), countMetLifeCategories(retryCategories))
			if metlifeProcedureCategoriesOK(retryCategories) {
				spool.Notes = append(spool.Notes, fmt.Sprintf("procedure categories retried with employeeId=%s", fallbackEmployeeID))
				spool.ProcedureCategoriesRequest = &retryReq
				categories = retryCategories
				categoriesEmployeeID = fallbackEmployeeID
				break
			}
		}
	}
	spool.ProcedureCategories = categories
	log.Printf("[MetLife] api procedureCategories patNum=%s aptNum=%s status=%s employeeId=%s insureds=%d categories=%d",
		appointment.PatNum, appointment.AptNum, metlifeOutcomeStatus(categories), categoriesEmployeeID, len(categories.Insureds), countMetLifeCategories(categories))

	var selectedProvider *metlifeapi.Provider
	if strings.TrimSpace(input.Credential.ProviderTIN) == "" {
		spool.Notes = append(spool.Notes, "provider TIN missing; providers/procedureCodes skipped until server config wiring is enabled")
		log.Printf("[MetLife] provider TIN missing patNum=%s aptNum=%s - skipping providers/procedureCodes",
			appointment.PatNum, appointment.AptNum)
	} else {
		providersReq := &metlifeapi.ProvidersRequest{
			ActualID:       coveredPerson.ActualID,
			CustomerNumber: coveredPerson.CustomerNumber,
			SSN:            providerLookupSSN(coveredPerson),
			TIN:            strings.TrimSpace(input.Credential.ProviderTIN),
		}
		spool.ProvidersRequest = providersReq
		providers, providersErr := probe.FetchProviders(ctx, *providersReq)
		if providersErr != nil {
			spool.Notes = append(spool.Notes, "providers lookup failed: "+providersErr.Error())
			log.Printf("[MetLife] api providers patNum=%s aptNum=%s status=error err=%v",
				appointment.PatNum, appointment.AptNum, providersErr)
		} else {
			spool.Providers = providers
			log.Printf("[MetLife] api providers patNum=%s aptNum=%s status=ok providers=%d",
				appointment.PatNum, appointment.AptNum, len(providers.Providers))
			selectedProvider = selectProvider(providers.Providers, input.Credential.ProviderName)
			if selectedProvider != nil {
				spool.SelectedProvider = selectedProvider
				if len(categories.Insureds) > 0 {
					keyNum := pickKeyNum(categories.Insureds[0])
					for _, categoryCode := range collectProcedureCategoryCodes(categories) {
						req := metlifeapi.ProcedureCodesRequest{
							Branch:                  coveredPerson.Branch,
							DependentSequenceNumber: coveredPerson.DependentSequenceNumber,
							EmployeeID:              categoriesEmployeeID,
							Group:                   coveredPerson.GroupNumber,
							KeyNum:                  keyNum,
							PPOInd:                  procedureCodesPPOInd,
							ProcedureCategory:       categoryCode,
							ProviderFirstInitial:    selectedProvider.ProviderKey.FirstInitial,
							ProviderLastname:        selectedProvider.ProviderKey.LastName,
							ProviderPhone:           selectedProvider.ProviderKey.Phone,
							ProviderState:           selectedProvider.ProviderKey.State,
							ProviderTin:             strings.TrimSpace(input.Credential.ProviderTIN),
							ProviderUnique:          strconv.Itoa(selectedProvider.ProviderKey.UniqueNumber),
							ProviderZipcode:         normalizeZipCode(selectedProvider.ZipCode),
							RelationshipCode:        coveredPerson.RelationShipCode,
							SubDivision:             coveredPerson.SubDivision,
						}
						spool.ProcedureCodeRequests = append(spool.ProcedureCodeRequests, req)
						resp, procErr := probe.FetchProcedureCodes(ctx, req)
						if procErr != nil {
							spool.Notes = append(spool.Notes, fmt.Sprintf("procedure code lookup failed for %s: %v", categoryCode, procErr))
							log.Printf("[MetLife] api procedureCodes patNum=%s aptNum=%s status=error category=%s err=%v",
								appointment.PatNum, appointment.AptNum, categoryCode, procErr)
							continue
						}
						spool.ProcedureCodeResponses[categoryCode] = resp
						log.Printf("[MetLife] api procedureCodes patNum=%s aptNum=%s status=ok category=%s procedures=%d",
							appointment.PatNum, appointment.AptNum, categoryCode, len(resp.Procedures))
					}
				}
			} else {
				spool.Notes = append(spool.Notes, "no provider matched current config; procedureCodes skipped")
			}
		}
	}

	return spool, nil
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

func writeEligibilityResult(outputDir string, appointment models.Appointment, el *eligibility.PatientEligibility, input payers.SessionInput) {
	if el == nil {
		return
	}
	filePath := filepath.Join(outputDir, fmt.Sprintf("%s_%s_eligibility.json",
		sanitizeSegment(appointment.PatNum),
		sanitizeSegment(appointment.AptNum),
	))
	if err := os.MkdirAll(filepath.Dir(filePath), 0o755); err != nil {
		log.Printf("[MetLife] create eligibility artifact dir failed patNum=%s: %v", appointment.PatNum, err)
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
		log.Printf("[MetLife] marshal eligibility failed patNum=%s: %v", appointment.PatNum, err)
		return
	}
	if err := os.WriteFile(filePath, data, 0o644); err != nil {
		log.Printf("[MetLife] write eligibility failed patNum=%s: %v", appointment.PatNum, err)
	}
}

func writeProbeResult(outputDir string, appointment models.Appointment, spool *probeSpool) {
	if spool == nil {
		return
	}
	path := filepath.Join(outputDir, fmt.Sprintf("%s_%s_probe.json",
		sanitizeSegment(appointment.PatNum), sanitizeSegment(appointment.AptNum)))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		log.Printf("[MetLife] create probe artifact dir failed patNum=%s aptNum=%s: %v",
			appointment.PatNum, appointment.AptNum, err)
		return
	}
	data, err := json.MarshalIndent(spool, "", "  ")
	if err != nil {
		log.Printf("[MetLife] write probe marshal failed patNum=%s aptNum=%s: %v",
			appointment.PatNum, appointment.AptNum, err)
		return
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		log.Printf("[MetLife] write probe failed patNum=%s aptNum=%s: %v",
			appointment.PatNum, appointment.AptNum, err)
	}
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
		log.Printf("[MetLife] marshal probe error failed patNum=%s aptNum=%s: %v",
			appointment.PatNum, appointment.AptNum, err)
		return
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		log.Printf("[MetLife] write probe error failed patNum=%s aptNum=%s: %v",
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
	if spool == nil || spool.CoveredPerson == nil {
		return nil
	}
	return metlifeeligibility.BuildFromProbe(
		spool.Appointment,
		spool.CoveredPerson,
		spool.PlanOverview,
		spool.ProcedureCategories,
		spool.ProcedureCodeResponses,
	)
}

func buildReportFromEligibility(
	appointment models.Appointment,
	el *eligibility.PatientEligibility,
	officeCodes []string,
	tpCodes []string,
) *advanced.PatientEligibilityReport {
	switch {
	case el == nil:
		return payers.BuildNotFoundReport(appointment)
	case !el.Patient.IsEligible:
		r := payers.BuildNotActiveReport(appointment, el.Plan.PlanName, el.Plan.Carrier, el.Plan.GroupName)
		r.Patient.FullName = el.Patient.FullName
		r.Patient.MemberID = el.Patient.MemberID
		r.Patient.DateOfBirth = el.Patient.DateOfBirth
		r.Patient.GroupNumber = el.Patient.GroupNumber
		return r
	default:
		report := advanced.Build(el, officeCodes, tpCodes)
		if report == nil {
			return payers.BuildUnableToDetermineReport(appointment)
		}
		return report
	}
}

func buildReportFromSpool(
	spool *probeSpool,
	el *eligibility.PatientEligibility,
	officeCodes []string,
	tpCodes []string,
) *advanced.PatientEligibilityReport {
	if spool == nil {
		return nil
	}
	if spool.CoveredPerson == nil {
		r := payers.BuildNotFoundReport(spool.Appointment)
		r.Source = "MetLifeAPIProbe"
		if spool.Overview != nil && strings.TrimSpace(spool.Overview.ReasonMessage) != "" {
			r.StatusReason = "MetLife overview returned: " + strings.TrimSpace(spool.Overview.ReasonMessage) + "."
		}
		return r
	}
	if strings.EqualFold(strings.TrimSpace(spool.CoveredPerson.CoverageStatus), "active") &&
		(spool.PlanOverview == nil || spool.ProcedureCategories == nil) {
		return payers.BuildUnableToDetermineReport(spool.Appointment)
	}
	return buildReportFromEligibility(spool.Appointment, el, officeCodes, tpCodes)
}

func writeAdvancedResult(outputDir string, appointment models.Appointment, report *advanced.PatientEligibilityReport) {
	if report == nil {
		return
	}
	path := filepath.Join(outputDir, fmt.Sprintf("%s_%s_advanced.json",
		sanitizeSegment(appointment.PatNum), sanitizeSegment(appointment.AptNum)))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		log.Printf("[MetLife] create advanced artifact dir failed patNum=%s aptNum=%s: %v",
			appointment.PatNum, appointment.AptNum, err)
		return
	}
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		log.Printf("[MetLife] write advanced marshal failed patNum=%s aptNum=%s: %v",
			appointment.PatNum, appointment.AptNum, err)
		return
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		log.Printf("[MetLife] write advanced failed patNum=%s aptNum=%s: %v",
			appointment.PatNum, appointment.AptNum, err)
	}
}

func selectCoveredPerson(persons []metlifeapi.CoveredPerson, appointment models.Appointment) *metlifeapi.CoveredPerson {
	if len(persons) == 0 {
		return nil
	}

	targetDOB := normalizeISODate(strings.TrimSpace(appointment.DOB))
	targetFirst := strings.ToLower(strings.TrimSpace(appointment.FName))
	targetLast := cleanLower(firstNonEmpty(strings.TrimSpace(appointment.LName), strings.TrimSpace(appointment.SubLName)))
	targetSubscriberLast := cleanLower(strings.TrimSpace(appointment.SubLName))
	targetSubscriberZip := normalizeZipCode(appointment.SubZip)
	targetSubID := strings.TrimSpace(appointment.SubscriberID)

	bestIdx := -1
	bestScore := -1
	for i := range persons {
		p := persons[i]
		score := 0
		if strings.EqualFold(strings.TrimSpace(p.CoverageStatus), "active") {
			score += 5
		}
		if targetSubID != "" && strings.TrimSpace(p.EmployeeID) == targetSubID {
			score += 3
		}
		if targetDOB != "" && normalizeISODate(p.DateOfBirth) == targetDOB {
			score += 6
		}
		if targetFirst != "" && cleanLower(p.FirstName) == targetFirst {
			score += 3
		}
		if targetLast != "" && cleanLower(p.LastName) == targetLast {
			score += 2
		}
		if targetSubscriberLast != "" && cleanLower(p.LastName) == targetSubscriberLast {
			score++
		}
		if targetSubscriberZip != "" && normalizeZipCode(p.Zip) == targetSubscriberZip {
			score++
		}
		if score > bestScore {
			bestScore = score
			bestIdx = i
		}
	}
	if bestIdx < 0 {
		return nil
	}
	return &persons[bestIdx]
}

func buildOverviewRequest(appointment models.Appointment) metlifeapi.EligibilityOverviewRequest {
	return metlifeapi.EligibilityOverviewRequest{
		EmployeeID:   strings.TrimSpace(appointment.SubscriberID),
		PlanTypeCode: defaultPlanTypeCode,
		LastName: firstNonEmpty(
			strings.TrimSpace(appointment.SubLName),
			strings.TrimSpace(appointment.LName),
		),
		ZipCode: normalizeZipCode(appointment.SubZip),
	}
}

func shouldRetryOverviewWithoutZip(overview *metlifeapi.EligibilityOverviewResponse, req metlifeapi.EligibilityOverviewRequest) bool {
	if overview == nil || strings.TrimSpace(req.ZipCode) == "" || len(overview.CoveredPersons) > 0 {
		return false
	}
	reason := strings.ToLower(strings.TrimSpace(overview.ReasonMessage))
	return strings.Contains(reason, "zip")
}

func metlifeEmployeeID(person *metlifeapi.CoveredPerson, appointment models.Appointment) string {
	if person == nil {
		return strings.TrimSpace(appointment.SubscriberID)
	}
	return firstNonEmpty(
		strings.TrimSpace(appointment.SubscriberID),
		strings.TrimSpace(person.ActualID),
		strings.TrimSpace(person.EmployeeID),
	)
}

func metlifeCoveredPersonEmployeeID(person *metlifeapi.CoveredPerson, appointment models.Appointment) string {
	if person == nil {
		return strings.TrimSpace(appointment.SubscriberID)
	}
	return firstNonEmpty(
		strings.TrimSpace(person.EmployeeID),
		strings.TrimSpace(person.ActualID),
		strings.TrimSpace(appointment.SubscriberID),
	)
}

func selectProvider(providers []metlifeapi.Provider, providerName string) *metlifeapi.Provider {
	if len(providers) == 0 {
		return nil
	}
	target := cleanLower(providerName)
	if target != "" {
		for i := range providers {
			if cleanLower(providers[i].ProviderName) == target {
				return &providers[i]
			}
		}
		for i := range providers {
			fullName := cleanLower(strings.TrimSpace(providers[i].FirstName + " " + providers[i].LastName))
			if fullName == target {
				return &providers[i]
			}
		}
		for i := range providers {
			if strings.Contains(cleanLower(providers[i].ProviderName), target) {
				return &providers[i]
			}
		}
	}

	preferred := slices.IndexFunc(providers, func(p metlifeapi.Provider) bool {
		return strings.EqualFold(strings.TrimSpace(p.FirstName), "RACHNA") &&
			strings.EqualFold(strings.TrimSpace(p.LastName), "SURANA")
	})
	if preferred >= 0 {
		return &providers[preferred]
	}
	return &providers[0]
}

func collectProcedureCategoryCodes(resp *metlifeapi.ProcedureCategoriesResponse) []string {
	if resp == nil {
		return nil
	}
	seen := make(map[string]struct{})
	var codes []string
	for _, insured := range resp.Insureds {
		for _, group := range insured.ProcedureCategoryGroups {
			for _, category := range group.ProcedureCategories {
				code := strings.TrimSpace(category.TypeCode)
				if code == "" {
					continue
				}
				if _, ok := seen[code]; ok {
					continue
				}
				seen[code] = struct{}{}
				codes = append(codes, code)
			}
		}
	}
	slices.Sort(codes)
	return codes
}

func pickKeyNum(insured metlifeapi.ProcedureCategoriesInsured) string {
	if strings.TrimSpace(insured.InNetworkKeyNumber) != "" {
		return strings.TrimSpace(insured.InNetworkKeyNumber)
	}
	return strings.TrimSpace(insured.OutNetworkKeyNumber)
}

func providerLookupSSN(person *metlifeapi.CoveredPerson) string {
	if person == nil {
		return ""
	}
	ssnDigits := digitsOnly(person.SSN)
	if ssnDigits != "" && strings.Trim(ssnDigits, "0") != "" {
		return ssnDigits
	}
	actualID := digitsOnly(person.ActualID)
	if actualID == "" {
		return ""
	}
	if len(actualID) >= 11 {
		return actualID
	}
	return strings.Repeat("0", 11-len(actualID)) + actualID
}

func normalizeZipCode(zip string) string {
	zip = strings.TrimSpace(zip)
	if idx := strings.Index(zip, "-"); idx >= 0 {
		zip = zip[:idx]
	}
	return zip
}

func normalizeISODate(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	for _, layout := range []string{"2006-01-02", "01/02/2006", "1/2/2006", "01-02-2006"} {
		if t, err := time.Parse(layout, value); err == nil {
			return t.Format("2006-01-02")
		}
	}
	return value
}

func memberType(relationshipCode, relationship string) string {
	if strings.EqualFold(strings.TrimSpace(relationship), "dependent") {
		return "Dependent"
	}
	switch strings.TrimSpace(relationshipCode) {
	case "0":
		return "Subscriber"
	default:
		return "Dependent"
	}
}

func cleanLower(value string) string {
	return strings.ToLower(strings.Join(strings.Fields(strings.TrimSpace(value)), " "))
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func digitsOnly(value string) string {
	var b strings.Builder
	for _, r := range value {
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func sanitizeSegment(s string) string {
	return strings.NewReplacer("/", "_", "\\", "_", ":", "_", " ", "_").Replace(s)
}
