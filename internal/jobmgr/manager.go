package jobmgr

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"insurance-benefit-agent-go/internal/appointments"
	"insurance-benefit-agent-go/internal/cache"
	"insurance-benefit-agent-go/internal/config"
	"insurance-benefit-agent-go/internal/controlplane"
	"insurance-benefit-agent-go/internal/mfa"
	"insurance-benefit-agent-go/internal/models"
	"insurance-benefit-agent-go/internal/odetrans"
	"insurance-benefit-agent-go/internal/officecodes"
	"insurance-benefit-agent-go/internal/payers"
	"insurance-benefit-agent-go/internal/pdf"
	"insurance-benefit-agent-go/internal/resultwriter"
)

type Manager struct {
	cfg         *config.Config
	control     *controlplane.Client
	registry    *payers.Registry
	cache       *cache.Cache
	officeCodes *officecodes.Service
	appts       *appointments.Selector
	runMu       sync.Mutex
	runSeq      atomic.Uint64
	stateMu     sync.RWMutex
	state       RunState
}

var ErrRunInProgress = errors.New("run already in progress")

type TriggerRequest struct {
	Action         string
	PatNum         string
	PatNums        []string
	PatientTargets []PatientTarget
	AddDays        int
	RequestedBy    string
	Appointments   []models.Appointment
	OfficeIdentity odetrans.OfficeIdentity
}

type PatientTarget struct {
	PatNum string `json:"patnum"`
	AptNum string `json:"aptnum,omitempty"`
}

type RunState struct {
	Running         bool            `json:"running"`
	RunID           string          `json:"runId,omitempty"`
	Action          string          `json:"action,omitempty"`
	PatNum          string          `json:"patnum,omitempty"`
	PatNums         []string        `json:"patnums,omitempty"`
	PatientTargets  []PatientTarget `json:"data,omitempty"`
	AddDays         int             `json:"addDays,omitempty"`
	RequestedBy     string          `json:"requestedBy,omitempty"`
	StartedAt       *time.Time      `json:"startedAt,omitempty"`
	LastCompletedAt *time.Time      `json:"lastCompletedAt,omitempty"`
	LastError       string          `json:"lastError,omitempty"`
}

type deferredPDFQueue struct {
	mu    sync.Mutex
	tasks []payers.DeferredPDFTask
}

func (q *deferredPDFQueue) Enqueue(task payers.DeferredPDFTask) {
	if q == nil || !task.WritePDF || task.Report == nil {
		return
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	q.tasks = append(q.tasks, task)
}

func (q *deferredPDFQueue) Snapshot() []payers.DeferredPDFTask {
	if q == nil {
		return nil
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	tasks := make([]payers.DeferredPDFTask, len(q.tasks))
	copy(tasks, q.tasks)
	return tasks
}

func New(cfg *config.Config, control *controlplane.Client, registry *payers.Registry) *Manager {
	return &Manager{
		cfg:         cfg,
		control:     control,
		registry:    registry,
		cache:       cache.New(),
		officeCodes: officecodes.New(),
		appts:       appointments.NewSelector(cfg.Sweep.QueryLimit),
	}
}

func (m *Manager) Run(ctx context.Context, runOnce bool) error {
	if runOnce {
		return m.runBlocking(ctx, TriggerRequest{
			Action:      "run_all",
			AddDays:     1,
			RequestedBy: "cli",
		})
	}
	log.Printf("manager idle: waiting for external trigger")
	<-ctx.Done()
	return nil
}

func (m *Manager) Trigger(ctx context.Context, req TriggerRequest) (string, error) {
	req = normalizeTriggerRequest(req)
	if err := validateTriggerRequest(req); err != nil {
		return "", err
	}

	// Dedup: same action/params already running.
	if len(req.Appointments) == 0 && m.isDuplicateRunning(req) {
		m.stateMu.RLock()
		runID := m.state.RunID
		m.stateMu.RUnlock()
		log.Printf("trigger deduplicated (running): action=%s patNum=%s addDays=%d", req.Action, req.PatNum, req.AddDays)
		return runID, nil
	}

	// Dedup: same action/params already pending in queue.
	if len(req.Appointments) == 0 {
		if existingID, dup := m.isDuplicateQueued(req); dup {
			log.Printf("trigger deduplicated (queued): action=%s patNum=%s addDays=%d existingId=%s", req.Action, req.PatNum, req.AddDays, existingID)
			return existingID, nil
		}
	}

	runID := m.nextRunID()

	if !m.runMu.TryLock() {
		// Busy: write a pending queue file; appointments are filled in when the run starts.
		run := QueuedRun{
			RunID:              runID,
			Action:             req.Action,
			PatNum:             req.PatNum,
			PatNums:            req.PatNums,
			PatientTargets:     req.PatientTargets,
			AddDays:            req.AddDays,
			RequestedBy:        req.RequestedBy,
			ReceivedAt:         time.Now().UTC(),
			SourceAppointments: req.Appointments,
		}
		if err := persistQueueFile(defaultQueueDir, run); err != nil {
			log.Printf("queue file write failed runId=%s: %v", runID, err)
		} else {
			log.Printf("trigger queued: runId=%s action=%s patNum=%s addDays=%d", runID, req.Action, req.PatNum, req.AddDays)
		}
		return runID, nil
	}

	m.markStarted(runID, req)
	go m.runAsync(ctx, runID, req)
	return runID, nil
}

func (m *Manager) isDuplicateRunning(req TriggerRequest) bool {
	m.stateMu.RLock()
	defer m.stateMu.RUnlock()
	if !m.state.Running {
		return false
	}
	return m.state.Action == req.Action &&
		m.state.PatNum == req.PatNum &&
		strings.Join(m.state.PatNums, ",") == strings.Join(req.PatNums, ",") &&
		targetsKey(m.state.PatientTargets) == targetsKey(req.PatientTargets) &&
		m.state.AddDays == req.AddDays
}

func (m *Manager) isDuplicateQueued(req TriggerRequest) (string, bool) {
	runs, err := loadPendingQueueFiles(defaultQueueDir)
	if err != nil || len(runs) == 0 {
		return "", false
	}
	for _, run := range runs {
		if len(run.SourceAppointments) > 0 {
			continue
		}
		if run.Action == req.Action && run.PatNum == req.PatNum && strings.Join(run.PatNums, ",") == strings.Join(req.PatNums, ",") && targetsKey(run.PatientTargets) == targetsKey(req.PatientTargets) && run.AddDays == req.AddDays {
			return run.RunID, true
		}
	}
	return "", false
}

func (m *Manager) Status() RunState {
	m.stateMu.RLock()
	defer m.stateMu.RUnlock()
	return cloneRunState(m.state)
}

func (m *Manager) runIfIdle(ctx context.Context, req TriggerRequest) error {
	req = normalizeTriggerRequest(req)
	if err := validateTriggerRequest(req); err != nil {
		return err
	}
	if !m.runMu.TryLock() {
		return ErrRunInProgress
	}
	defer m.runMu.Unlock()

	runID := m.nextRunID()
	m.markStarted(runID, req)
	err := m.runRequest(ctx, runID, req)
	m.markCompleted(err)
	return err
}

func (m *Manager) runBlocking(ctx context.Context, req TriggerRequest) error {
	req = normalizeTriggerRequest(req)
	if err := validateTriggerRequest(req); err != nil {
		return err
	}
	m.runMu.Lock()
	defer m.runMu.Unlock()

	runID := m.nextRunID()
	m.markStarted(runID, req)
	err := m.runRequest(ctx, runID, req)
	m.markCompleted(err)
	return err
}

func (m *Manager) runAsync(ctx context.Context, runID string, req TriggerRequest) {
	defer m.runMu.Unlock()
	err := m.runRequest(ctx, runID, req)
	if err != nil {
		log.Printf("triggered run failed: runId=%s action=%s patnum=%s addDays=%d err=%v", runID, req.Action, req.PatNum, req.AddDays, err)
	} else {
		log.Printf("triggered run completed: runId=%s action=%s patnum=%s addDays=%d", runID, req.Action, req.PatNum, req.AddDays)
	}
	m.markCompleted(err)
}

func (m *Manager) runRequest(ctx context.Context, runID string, req TriggerRequest) error {
	log.Printf("run requested: action=%s patnum=%s addDays=%d requestedBy=%s", req.Action, req.PatNum, req.AddDays, req.RequestedBy)
	snapshot, err := m.prepareSnapshot(ctx)
	if err != nil {
		return err
	}

	officeCodes, err := m.ensureOfficeCodes(ctx, snapshot.ScraperConfig)
	if err != nil {
		return err
	}

	switch req.Action {
	case "run_now":
		return m.runNow(ctx, runID, req, snapshot, officeCodes)
	case "run_all":
		return m.runAll(ctx, runID, req, snapshot, officeCodes)
	default:
		return fmt.Errorf("unsupported action %q", req.Action)
	}
}

func (m *Manager) prepareSnapshot(ctx context.Context) (*cache.WorkSnapshot, error) {
	snapshot, err := m.fetchFreshSnapshot(ctx)
	if err == nil {
		if err := m.cache.SaveSnapshot(snapshot); err != nil {
			return nil, err
		}
		return snapshot, nil
	}

	log.Printf("fresh snapshot failed: %v", err)
	cached, cacheErr := m.cache.LoadValidSnapshot(m.cfg.OfficeKey)
	if cacheErr != nil {
		return nil, fmt.Errorf("fresh snapshot failed (%v); cached snapshot unavailable: %w", err, cacheErr)
	}

	log.Printf("using cached snapshot from %s with %d payers", cached.CreatedAt.Format(time.RFC3339Nano), len(cached.Payers))
	return cached, nil
}

func (m *Manager) runNow(ctx context.Context, runID string, req TriggerRequest, snapshot *cache.WorkSnapshot, officeCodes []string) error {
	activePayers := m.activePayers(snapshot.Payers)
	rows := dedupeAppointmentsByPatNumOrdinal(req.Appointments)
	if len(rows) == 0 {
		var err error
		rows, err = m.appts.SelectForPatients(ctx, appointments.PatientSelectRequest{
			OfficeKey:     snapshot.OfficeKey,
			PatNums:       req.PatNums,
			Targets:       appointmentPatientTargets(req.PatientTargets),
			ScraperConfig: snapshot.ScraperConfig,
		})
		if err != nil {
			return err
		}
		rows = dedupeAppointmentsByPatNumOrdinal(rows)
	}
	if len(rows) == 0 {
		msg := fmt.Sprintf("no insurance rows found for patnum=%s", req.PatNum)
		m.notifyRunNowNoRows(ctx, snapshot, runID, req, req.PatNums, runNowMissingInfoMessage)
		return fmt.Errorf("%s", msg)
	}
	if missing := missingPatNums(req.PatNums, rows); len(missing) > 0 {
		log.Printf("run_now missing patient rows: runId=%s patnums=%s", runID, strings.Join(missing, ","))
	}

	buckets := bucketAppointmentsByPayer(activePayers, rows)
	logPayerBucketSummary("run_now", req.PatNum, 0, activePayers, buckets)
	logUnsupportedPayerRows("run_now", req.PatNum, 0, buckets.Unsupported)
	if len(buckets.Supported) == 0 {
		m.notifyRunNowNoRows(ctx, snapshot, runID, req, unsupportedPatNumsFromBuckets(buckets.Unsupported), runNowMissingInfoMessage)
		return fmt.Errorf("unsupported_payer: patnum=%s", req.PatNum)
	}

	// Write queue file with full appointment list BEFORE opening any browser.
	var queuedAppts []QueuedAppointment
	for _, payer := range activePayers {
		for _, a := range buckets.Supported[payer.PayerURL] {
			queuedAppts = append(queuedAppts, QueuedAppointment{PayerURL: payer.PayerURL, Appointment: a})
		}
	}
	run := m.buildQueuedRun(runID, req, queuedAppts)
	run.Phase = PhaseProbing
	if err := persistQueueFile(defaultQueueDir, run); err != nil {
		log.Printf("queue file write failed runId=%s: %v", runID, err)
	}

	pdfQueue := &deferredPDFQueue{}
	run = m.processPhase1WithFallback(ctx, snapshot, activePayers, officeCodes, buckets, run, pdfQueue, fmt.Sprintf("patnum=%s", req.PatNum), req.OfficeIdentity)
	if err := persistQueueFile(defaultQueueDir, run); err != nil {
		log.Printf("queue file update failed runId=%s: %v", runID, err)
	}
	m.drainDeferredPDFs(ctx, snapshot, pdfQueue)
	m.finalizeProbing(ctx, run, snapshot, officeCodes)
	return nil
}

func (m *Manager) runAll(ctx context.Context, runID string, req TriggerRequest, snapshot *cache.WorkSnapshot, officeCodes []string) error {
	activePayers := m.activePayers(snapshot.Payers)
	if m.cfg.Local.FlagBool("skipProbing", false) {
		log.Printf("run_all skipProbing=true: running Phase 2 directly from probe bucket")
		pdfQueue := &deferredPDFQueue{}
		for _, payer := range activePayers {
			if _, err := m.processPayerAppointmentsPostProcess(ctx, snapshot, payer, officeCodes, []models.Appointment{}, pdfQueue, req.OfficeIdentity); err != nil {
				log.Printf("skipProbing postprocess failed: payerUrl=%s: %v", payer.PayerURL, err)
			}
		}
		m.drainDeferredPDFs(ctx, snapshot, pdfQueue)
		return nil
	}

	rows := dedupeAppointmentsByPatNumOrdinal(req.Appointments)
	if len(rows) == 0 {
		var err error
		rows, err = m.appts.SelectForDay(ctx, appointments.DaySelectRequest{
			OfficeKey:                     snapshot.OfficeKey,
			PayerIDs:                      collectPayerIDs(activePayers),
			AddDays:                       req.AddDays,
			RetryErrorsOnly:               true,
			IgnoreAppointmentStatusFilter: m.cfg.Testing.ShouldUseAllAppointments(),
			ScraperConfig:                 snapshot.ScraperConfig,
		})
		if err != nil {
			return err
		}
		rows = dedupeAppointmentsByPatNumOrdinal(rows)
	}

	buckets := bucketAppointmentsByPayer(activePayers, rows)
	logPayerBucketSummary("run_all", "", req.AddDays, activePayers, buckets)
	logUnsupportedPayerRows("run_all", "", req.AddDays, buckets.Unsupported)

	// Write queue file with full appointment list BEFORE opening any browser.
	var queuedAppts []QueuedAppointment
	for _, payer := range activePayers {
		for _, a := range buckets.Supported[payer.PayerURL] {
			queuedAppts = append(queuedAppts, QueuedAppointment{PayerURL: payer.PayerURL, Appointment: a})
		}
	}
	run := m.buildQueuedRun(runID, req, queuedAppts)
	run.Phase = PhaseProbing
	if err := persistQueueFile(defaultQueueDir, run); err != nil {
		log.Printf("queue file write failed runId=%s: %v", runID, err)
	}

	pdfQueue := &deferredPDFQueue{}
	run = m.processPhase1WithFallback(ctx, snapshot, activePayers, officeCodes, buckets, run, pdfQueue, fmt.Sprintf("addDays=%d", req.AddDays), req.OfficeIdentity)
	if err := persistQueueFile(defaultQueueDir, run); err != nil {
		log.Printf("queue file update failed runId=%s: %v", runID, err)
	}
	m.drainDeferredPDFs(ctx, snapshot, pdfQueue)
	m.finalizeProbing(ctx, run, snapshot, officeCodes)
	return nil
}

func (m *Manager) fetchFreshSnapshot(ctx context.Context) (*cache.WorkSnapshot, error) {
	serverConfig, err := m.control.FetchServerConfig(ctx)
	if err != nil {
		return nil, err
	}
	return MapServerConfig(m.cfg, serverConfig)
}

// MapServerConfig converts the patcon ServerConfig into a WorkSnapshot.
// Each payerConfig entry is already scoped to this office and carries credentials.
func MapServerConfig(cfg *config.Config, sc *models.ServerConfig) (*cache.WorkSnapshot, error) {
	payers := make([]models.Payer, 0, len(sc.PayerConfig))

	for _, p := range sc.PayerConfig {
		if p.PayerURL == "" {
			continue
		}
		payers = append(payers, models.Payer{
			ID:       p.ID,
			PayerURL: p.PayerURL,
			Name:     p.Name,
			PayerIDs: p.PayerIDs,
			Credential: models.CredentialCandidate{
				Username:     p.Username,
				Password:     p.Password,
				MFAMethod:    p.MFAMethod,
				MFAEmail:     p.MFAMeta.Email,
				ProviderName: p.MFAMeta.ProvName,
				ProviderTIN:  p.MFAMeta.ProvTIN,
			},
		})
	}

	if len(payers) == 0 {
		return nil, fmt.Errorf("server returned no payers")
	}

	mfaHost, mfaPort, mfaSecure := resolveIMAPHost(sc.MFAHost)
	if mfaPort == 0 {
		mfaPort = sc.MFAPort
	}
	mfaPass := sc.MFAPass
	if cfg.Local != nil && cfg.Local.Overrides.MFAPassword != nil {
		mfaPass = *cfg.Local.Overrides.MFAPassword
	}

	apptRangeDays := sc.QISAptRangeDays
	if apptRangeDays <= 0 {
		apptRangeDays = 10
	}

	// MAIN_DOMAIN/MAIN_API_TOKEN are the operational coordinates returned by the
	// server after bootstrap. They match the bootstrap URL today but allow the
	// server to redirect clients to a new host without touching agent.config.json.
	patconURL := sc.MainDomain
	if patconURL == "" {
		patconURL = cfg.Bootstrap.Patcon.URL
	}
	patconToken := sc.MainAPIToken
	if patconToken == "" {
		patconToken = cfg.Bootstrap.Patcon.Token
	}

	scraperConfig := &models.ScraperConfig{
		JobTTLSec:          86400,
		ScraperConcurrency: 1,
		APIs: map[string]any{
			"patcon": map[string]any{
				"url":   patconURL,
				"token": patconToken,
			},
			"query": map[string]any{
				"url":   strings.TrimRight(sc.QueryDomain, "/") + "/api/run/query",
				"token": sc.QueryToken,
			},
		},
		MFA: models.MFAConfig{
			Email: models.EmailMFAConfig{
				Host:           mfaHost,
				Port:           mfaPort,
				Secure:         mfaSecure,
				User:           sc.MFAUser,
				Password:       mfaPass,
				Mailbox:        "INBOX",
				TimeoutMS:      60000,
				PollIntervalMS: 3000,
			},
		},
		Office: models.OfficeConfig{
			OfficeKey:       cfg.OfficeKey,
			Active:          sc.IsActive == 1,
			ApptRangeDays:   apptRangeDays,
			InsPDFGenerate:  sc.InsPDFGenerate,
			SweepIntervalMs: sc.QISIntervalMs,
			SweepStartTime:  sc.QISStartTime,
		},
	}

	createdAt := time.Now().UTC()
	return &cache.WorkSnapshot{
		CreatedAt:     createdAt,
		ExpiresAt:     createdAt.Add(24 * time.Hour),
		OfficeKey:     cfg.OfficeKey,
		UserID:        sc.UserID,
		ConfigAPIURL:  sc.ConfigAPIURL,
		ScraperConfig: scraperConfig,
		Payers:        payers,
	}, nil
}

// resolveIMAPHost maps host shortcuts (e.g. "gmail") to full IMAP hostnames,
// the correct IMAPS port, and whether TLS is required.
func resolveIMAPHost(host string) (resolvedHost string, port int, secure bool) {
	switch strings.ToLower(strings.TrimSpace(host)) {
	case "gmail":
		return "imap.gmail.com", 993, true
	case "outlook", "hotmail":
		return "outlook.office365.com", 993, true
	default:
		return host, 0, false
	}
}

func (m *Manager) ensureOfficeCodes(ctx context.Context, scraperConfig *models.ScraperConfig) ([]string, error) {
	codes, err := m.officeCodes.GetOfficeCodes(ctx, m.cfg.OfficeKey, scraperConfig)
	if err != nil {
		return nil, fmt.Errorf("load office codes: %w", err)
	}
	return codes, nil
}

func (m *Manager) loopPayers(ctx context.Context, snapshot *cache.WorkSnapshot, officeCodes []string) error {
	pdfQueue := &deferredPDFQueue{}
	for _, payer := range snapshot.Payers {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		if m.cfg.Local.ShouldSkipPayer(payer.PayerURL) {
			log.Printf("skipping payer (local override): payerUrl=%s", payer.PayerURL)
			continue
		}

		if err := m.processPayer(ctx, snapshot, payer, officeCodes, pdfQueue); err != nil {
			log.Printf("payer failed: payerUrl=%s: %v", payer.PayerURL, err)
		}
	}
	m.drainDeferredPDFs(ctx, snapshot, pdfQueue)
	return nil
}

func (m *Manager) processPayerAppointments(ctx context.Context, snapshot *cache.WorkSnapshot, payer models.Payer, officeCodes []string, selected []models.Appointment, pdfQueue *deferredPDFQueue, identity odetrans.OfficeIdentity) error {
	if m.cfg.Local.ShouldSkipPayer(payer.PayerURL) {
		log.Printf("skipping payer (local override): payerUrl=%s", payer.PayerURL)
		return nil
	}
	sessionInput, err := m.buildPayerSessionInput(ctx, snapshot, payer, officeCodes, selected, identity)
	if err != nil {
		return err
	}
	if err := m.attachPayerRuntimeState(snapshot, sessionInput); err != nil {
		return err
	}
	_, err = m.runPayerSession(ctx, snapshot, payer, sessionInput, pdfQueue)
	return err
}

func (m *Manager) processPayer(ctx context.Context, snapshot *cache.WorkSnapshot, payer models.Payer, officeCodes []string, pdfQueue *deferredPDFQueue) error {
	if m.cfg.Local.ShouldSkipPayer(payer.PayerURL) {
		log.Printf("skipping payer (local override): payerUrl=%s", payer.PayerURL)
		return nil
	}
	sessionInput, err := m.buildPayerSessionInput(ctx, snapshot, payer, officeCodes, nil, odetrans.OfficeIdentity{})
	if err != nil {
		return err
	}
	if err := m.attachPayerRuntimeState(snapshot, sessionInput); err != nil {
		return err
	}
	_, err = m.runPayerSession(ctx, snapshot, payer, sessionInput, pdfQueue)
	return err
}

func (m *Manager) processPhase1WithFallback(ctx context.Context, snapshot *cache.WorkSnapshot, activePayers []models.Payer, officeCodes []string, buckets payerAppointmentBuckets, run QueuedRun, pdfQueue *deferredPDFQueue, logContext string, identity odetrans.OfficeIdentity) QueuedRun {
	fallbackPayer, hasFallback := firstWildcardPayer(activePayers)
	probeDir := m.currentProbeOutputDir()

	for _, payer := range activePayers {
		if hasFallback && strings.EqualFold(payer.PayerURL, fallbackPayer.PayerURL) {
			continue
		}
		appointmentsForPayer := buckets.Supported[payer.PayerURL]
		if len(appointmentsForPayer) == 0 {
			continue
		}
		err := m.processPayerAppointments(ctx, snapshot, payer, officeCodes, appointmentsForPayer, pdfQueue, identity)
		if err != nil {
			log.Printf("payer failed: payerUrl=%s %s: %v", payer.PayerURL, logContext, err)
		}
		if !hasFallback {
			continue
		}
		fallbacks := fallbackEligibleAppointments(probeDir, payer.PayerURL, appointmentsForPayer, err)
		if len(fallbacks) == 0 {
			continue
		}
		log.Printf("fallback queued: originalPayerUrl=%s fallbackPayerUrl=%s appointments=%d", payer.PayerURL, fallbackPayer.PayerURL, len(fallbacks))
		run = replaceQueuedAppointmentsWithFallback(run, payer.PayerURL, fallbackPayer.PayerURL, fallbackReason(err), fallbacks)
		buckets.Supported[fallbackPayer.PayerURL] = appendFallbackAppointments(buckets.Supported[fallbackPayer.PayerURL], fallbacks)
	}

	if hasFallback {
		appointmentsForPayer := buckets.Supported[fallbackPayer.PayerURL]
		if len(appointmentsForPayer) > 0 {
			if err := m.processPayerAppointments(ctx, snapshot, fallbackPayer, officeCodes, appointmentsForPayer, pdfQueue, identity); err != nil {
				log.Printf("fallback payer failed: payerUrl=%s %s: %v", fallbackPayer.PayerURL, logContext, err)
			}
		}
	}
	return run
}

func (m *Manager) activePayers(payers []models.Payer) []models.Payer {
	if len(payers) == 0 {
		return nil
	}
	out := make([]models.Payer, 0, len(payers))
	for _, payer := range payers {
		if m.cfg.Local.ShouldSkipPayer(payer.PayerURL) {
			log.Printf("skipping payer (local override): payerUrl=%s", payer.PayerURL)
			continue
		}
		out = append(out, payer)
	}
	return out
}

func (m *Manager) runPayerSession(ctx context.Context, snapshot *cache.WorkSnapshot, payer models.Payer, sessionInput *payers.SessionInput, pdfQueue *deferredPDFQueue) (payers.RunSummary, error) {
	startedAt := time.Now()

	// Start tracking for final patient outcomes only. Phase 1 probing produces
	// raw payer files; phase 2 postprocess converts them into statuses/counts.
	var trackingID string
	if sessionInput.SkipProbing && !m.cfg.Testing.ShouldSkipTracking() {
		if id, err := m.control.StartPayerTracking(ctx, snapshot.UserID, payer.ID); err != nil {
			log.Printf("start tracking failed payerUrl=%s: %v", payer.PayerURL, err)
		} else {
			trackingID = id
		}
	}

	if len(sessionInput.Appointments) == 0 && !sessionInput.SkipProbing {
		log.Printf("no appointments for payerUrl=%s, skipping", payer.PayerURL)
		m.endTracking(ctx, snapshot.UserID, trackingID, payer.ID, payer.PayerURL, "Skipped", 0, payers.RunSummary{}, "", startedAt)
		return payers.RunSummary{}, nil
	}
	if pdfQueue != nil {
		sessionInput.EnqueuePDF = pdfQueue.Enqueue
	}

	adapter, err := m.registry.GetForPayerURL(payer.PayerURL)
	if err != nil {
		m.endTracking(ctx, snapshot.UserID, trackingID, payer.ID, payer.PayerURL, "Failed", len(sessionInput.Appointments), payers.RunSummary{}, err.Error(), startedAt)
		return payers.RunSummary{}, err
	}

	total := len(sessionInput.Appointments)
	runSummary, runErr := adapter.Run(ctx, *sessionInput)
	if closeErr := pdf.CloseBrowser(); closeErr != nil {
		log.Printf("pdf browser close failed: %v", closeErr)
	}
	time.Sleep(2 * time.Second)
	if runErr != nil {
		log.Printf("payer run failed payerUrl=%s: %v", payer.PayerURL, runErr)
	}

	if runErr != nil {
		m.endTracking(ctx, snapshot.UserID, trackingID, payer.ID, payer.PayerURL, "Failed", total, runSummary, runErr.Error(), startedAt)
		return runSummary, runErr
	}

	m.endTracking(ctx, snapshot.UserID, trackingID, payer.ID, payer.PayerURL, "Complete", total, runSummary, "", startedAt)
	return runSummary, nil
}

func (m *Manager) drainDeferredPDFs(ctx context.Context, snapshot *cache.WorkSnapshot, queue *deferredPDFQueue) {
	tasks := queue.Snapshot()
	if len(tasks) == 0 {
		return
	}

	log.Printf("deferred pdf phase starting queued=%d", len(tasks))
	var writer *resultwriter.Writer
	if snapshot != nil && snapshot.ScraperConfig != nil {
		w, err := resultwriter.New(m.cfg.Testing, snapshot.ScraperConfig.APIs)
		if err != nil {
			log.Printf("deferred pdf resultwriter unavailable: %v", err)
		} else {
			writer = w
		}
	}

	pdfWriter := pdf.NewWriter()
	defer func() {
		if err := pdf.CloseBrowser(); err != nil {
			log.Printf("deferred pdf browser close failed: %v", err)
		}
	}()

	rendered := 0
	for _, task := range tasks {
		select {
		case <-ctx.Done():
			log.Printf("deferred pdf phase stopped: %v", ctx.Err())
			return
		default:
		}
		if !task.WritePDF || task.Report == nil {
			continue
		}

		pdfBytes, err := pdfWriter.WriteEligibilityPDF(task.Report)
		if err != nil {
			log.Printf("deferred pdf render failed payerUrl=%s patNum=%s aptNum=%s: %v",
				task.PayerURL, task.Appointment.PatNum, task.Appointment.AptNum, err)
			continue
		}
		writeDeferredSummaryPDF(task, pdfBytes)
		if writer != nil {
			writer.ApplyPDF(task.Appointment, task.Status, task.OfficeKey, pdfBytes, true)
		}
		rendered++
	}
	log.Printf("deferred pdf phase finished queued=%d rendered=%d", len(tasks), rendered)
}

func writeDeferredSummaryPDF(task payers.DeferredPDFTask, pdfBytes []byte) {
	if task.OutputDir == "" || len(pdfBytes) == 0 {
		return
	}
	if err := os.MkdirAll(task.OutputDir, 0o755); err != nil {
		log.Printf("deferred pdf artifact dir failed payerUrl=%s patNum=%s: %v", task.PayerURL, task.Appointment.PatNum, err)
		return
	}
	path := filepath.Join(task.OutputDir, fmt.Sprintf("%s_%s_summary.pdf",
		sanitizePDFSegment(task.Appointment.PatNum),
		sanitizePDFSegment(task.Appointment.AptNum),
	))
	if err := os.WriteFile(path, pdfBytes, 0o644); err != nil {
		log.Printf("deferred pdf artifact write failed payerUrl=%s patNum=%s path=%s: %v",
			task.PayerURL, task.Appointment.PatNum, path, err)
		return
	}
	log.Printf("deferred pdf artifact written payerUrl=%s patNum=%s aptNum=%s path=%s",
		task.PayerURL, task.Appointment.PatNum, task.Appointment.AptNum, path)
}

func sanitizePDFSegment(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "unknown"
	}
	var b strings.Builder
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z':
			b.WriteRune(r)
		case r >= 'A' && r <= 'Z':
			b.WriteRune(r)
		case r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '.', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}

func (m *Manager) endTracking(ctx context.Context, userID int, trackingID, payerID, payerURL, status string, total int, summary payers.RunSummary, runError string, startedAt time.Time) {
	if m.cfg.Testing.ShouldSkipTracking() || trackingID == "" {
		return
	}

	results := summary.Results
	if results == nil {
		results = []payers.PatientResult{}
	}
	report := map[string]any{
		"payerUrl":          payerURL,
		"runId":             m.currentRunID(),
		"startedAt":         startedAt.UTC().Format(time.RFC3339),
		"durationSec":       int(time.Since(startedAt).Seconds()),
		"TotalAppointments": total,
		"Verified":          summary.Verified,
		"Inactive":          summary.Inactive,
		"NotFound":          summary.NotFound,
		"PatientError":      summary.PatientError,
		"appointments":      results,
	}
	if runError != "" {
		report["runError"] = runError
	}

	if err := m.control.EndPayerTracking(ctx, userID, trackingID, payerID, status, report); err != nil {
		log.Printf("end tracking failed: %v", err)
	} else {
		log.Printf("tracking ended %s: verified=%d inactive=%d notFound=%d errors=%d",
			payerURL, summary.Verified, summary.Inactive, summary.NotFound, summary.PatientError)
	}
}

func (m *Manager) currentRunID() string {
	m.stateMu.RLock()
	defer m.stateMu.RUnlock()
	return m.state.RunID
}

func nilIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func (m *Manager) buildPayerSessionInput(ctx context.Context, snapshot *cache.WorkSnapshot, payer models.Payer, officeCodes []string, selected []models.Appointment, providedIdentity odetrans.OfficeIdentity) (*payers.SessionInput, error) {
	credential := payer.Credential

	if snapshot.ScraperConfig == nil {
		return nil, fmt.Errorf("snapshot scraper config is missing")
	}
	if credential.Password == "" {
		return nil, fmt.Errorf("credential password missing for payerUrl=%s", payer.PayerURL)
	}
	if len(payer.PayerIDs) == 0 {
		return nil, fmt.Errorf("payer has no payer IDs: payerUrl=%s", payer.PayerURL)
	}

	var emailMFA *mfa.EmailConfig
	if strings.EqualFold(credential.MFAMethod, "email") {
		cfg := snapshot.ScraperConfig.MFA.Email
		if cfg.Password == "" {
			return nil, fmt.Errorf("email MFA requested for payerUrl=%s but MFA mailbox password is missing", payer.PayerURL)
		}
		emailMFA = &mfa.EmailConfig{
			Host:            cfg.Host,
			Port:            cfg.Port,
			Secure:          cfg.Secure,
			User:            cfg.User,
			Password:        cfg.Password,
			ExpectedTo:      expectedMFAToAddress(cfg.User, credential.Username),
			DeleteAfterRead: true,
			CleanupMailbox:  "[Gmail]/Trash",
			Mailbox:         cfg.Mailbox,
			TimeoutMS:       cfg.TimeoutMS,
			PollIntervalMS:  cfg.PollIntervalMS,
		}
	}

	appointmentDays := snapshot.ScraperConfig.Office.ApptRangeDays
	if m.cfg.Testing.ApptRangeDays != nil {
		appointmentDays = *m.cfg.Testing.ApptRangeDays
	} else if appointmentDays <= 0 {
		if strings.EqualFold(payer.PayerURL, "DentaQuest.com") {
			appointmentDays = 3
		} else {
			appointmentDays = 10
		}
	}

	appointmentsForPayer := selected
	if appointmentsForPayer == nil {
		log.Printf("selecting appointments payerUrl=%s apptRangeDays=%d", payer.PayerURL, appointmentDays)
		var err error
		appointmentsForPayer, err = m.appts.SelectForPayer(ctx, appointments.SelectRequest{
			OfficeKey:                     snapshot.OfficeKey,
			PayerURL:                      payer.PayerURL,
			PayerIDs:                      payer.PayerIDs,
			FutureRangeDays:               appointmentDays,
			RetryErrorsOnly:               true,
			IgnoreAppointmentStatusFilter: m.cfg.Testing.ShouldUseAllAppointments(),
			ScraperConfig:                 snapshot.ScraperConfig,
		})
		if err != nil {
			return nil, fmt.Errorf("select appointments for payerUrl=%s: %w", payer.PayerURL, err)
		}
	}
	if m.cfg.Testing.MaxAppointments != nil && len(appointmentsForPayer) > *m.cfg.Testing.MaxAppointments {
		appointmentsForPayer = appointmentsForPayer[:*m.cfg.Testing.MaxAppointments]
	}

	insPDFGenerate := snapshot.ScraperConfig.Office.InsPDFGenerate
	if m.cfg.Local != nil && m.cfg.Local.Overrides.InsPDFGenerate != nil {
		insPDFGenerate = *m.cfg.Local.Overrides.InsPDFGenerate
	}
	var writePDF bool
	if m.cfg.Testing.WritePDF != nil {
		writePDF = *m.cfg.Testing.WritePDF
	} else if m.cfg.PDF.Enabled != nil {
		writePDF = *m.cfg.PDF.Enabled
	} else {
		writePDF = insPDFGenerate == 1
	}
	headless := m.cfg.Testing.IsHeadless(m.cfg.Local.IsHeadless(true))
	probeOutputDir := m.currentProbeOutputDir()
	officeIdentity, identityErr := odetrans.ResolveIdentity(ctx, snapshot.ScraperConfig, snapshot.OfficeKey, providedIdentity)
	if identityErr != nil {
		log.Printf("[ODEtrans] identity resolve failed payerUrl=%s: %v", payer.PayerURL, identityErr)
		officeIdentity = providedIdentity
	}

	return &payers.SessionInput{
		Payer:              payer,
		Credential:         credential,
		Password:           credential.Password,
		EmailMFA:           emailMFA,
		Appointments:       appointmentsForPayer,
		ScraperConfig:      snapshot.ScraperConfig,
		AppointmentDays:    appointmentDays,
		RequestedOfficeKey: snapshot.OfficeKey,
		Testing:            testingWithoutApptField(m.cfg.Testing),
		Headless:           headless,
		OfficeCodes:        officeCodes,
		WritePDF:           writePDF,
		AllowEDIWriteBack:  m.cfg.Testing.ShouldUpdateApptField(),
		ProbeOutputDir:     probeOutputDir,
		OfficeIdentity:     officeIdentity,
		SkipProbing:        m.cfg.Local.FlagBool("skipProbing", false),
	}, nil
}

func (m *Manager) currentProbeOutputDir() string {
	m.stateMu.RLock()
	startedAt := m.state.StartedAt
	m.stateMu.RUnlock()
	if startedAt == nil {
		return payers.ProbeRunDir(time.Now().UTC().Format("2006-01-02T15-04-05Z"))
	}
	return payers.ProbeRunDir(startedAt.UTC().Format("2006-01-02T15-04-05Z"))
}

func (m *Manager) attachPayerRuntimeState(snapshot *cache.WorkSnapshot, sessionInput *payers.SessionInput) error {
	if sessionInput == nil {
		return fmt.Errorf("session input is nil")
	}
	sessionInput.PatchCredentialFn = func(payerURL, providerName string) {
		for i := range snapshot.Payers {
			if snapshot.Payers[i].PayerURL == payerURL {
				snapshot.Payers[i].Credential.ProviderName = providerName
				break
			}
		}
		if err := m.cache.SaveSnapshot(snapshot); err != nil {
			log.Printf("failed to persist discovered providerName for %s: %v", payerURL, err)
		}
	}
	return nil
}

func expectedMFAToAddress(mailboxUser string, credentialUsername string) string {
	credentialUsername = strings.TrimSpace(credentialUsername)
	if credentialUsername == "" {
		return ""
	}
	if strings.Contains(credentialUsername, "@") {
		return strings.ToLower(credentialUsername)
	}

	localPart, domain, ok := strings.Cut(mailboxUser, "@")
	if !ok || localPart == "" || domain == "" {
		return ""
	}

	if strings.Contains(credentialUsername, "+") {
		return strings.ToLower(credentialUsername + "@" + domain)
	}
	if !strings.HasPrefix(strings.ToLower(credentialUsername), strings.ToLower(localPart)) {
		return ""
	}

	suffix := credentialUsername[len(localPart):]
	accountEnd := 0
	for accountEnd < len(suffix) && suffix[accountEnd] >= '0' && suffix[accountEnd] <= '9' {
		accountEnd++
	}
	if accountEnd == 0 || accountEnd == len(suffix) {
		return ""
	}

	account := suffix[:accountEnd]
	payerName := suffix[accountEnd:]
	return strings.ToLower(fmt.Sprintf("%s+%s+%s@%s", localPart, account, payerName, domain))
}

func normalizeTriggerRequest(req TriggerRequest) TriggerRequest {
	req.Action = strings.TrimSpace(strings.ToLower(req.Action))
	req.PatNum = strings.TrimSpace(req.PatNum)
	req.PatientTargets = normalizePatientTargets(req.PatientTargets)
	for _, target := range req.PatientTargets {
		req.PatNums = append(req.PatNums, target.PatNum)
	}
	req.PatNums = normalizePatNums(append(req.PatNums, req.PatNum))
	if len(req.PatientTargets) == 0 {
		for _, patNum := range req.PatNums {
			req.PatientTargets = append(req.PatientTargets, PatientTarget{PatNum: patNum})
		}
	}
	req.PatNum = strings.Join(req.PatNums, ",")
	req.RequestedBy = strings.TrimSpace(req.RequestedBy)
	if req.RequestedBy == "" {
		req.RequestedBy = "api"
	}
	req.Appointments = dedupeAppointmentsByPatNumOrdinal(req.Appointments)
	return req
}

func validateTriggerRequest(req TriggerRequest) error {
	switch req.Action {
	case "run_now":
		if len(req.PatNums) == 0 && len(req.PatientTargets) == 0 && len(req.Appointments) == 0 {
			return fmt.Errorf("patnum is required when action=run_now")
		}
		return nil
	case "run_all":
		if req.AddDays < 0 {
			return fmt.Errorf("addDays must be >= 0 when action=run_all")
		}
		return nil
	default:
		return fmt.Errorf("unsupported action %q", req.Action)
	}
}

func normalizePatientTargets(targets []PatientTarget) []PatientTarget {
	out := make([]PatientTarget, 0, len(targets))
	seen := map[string]struct{}{}
	for _, target := range targets {
		target.PatNum = strings.TrimSpace(target.PatNum)
		target.AptNum = strings.TrimSpace(target.AptNum)
		if target.PatNum == "" {
			continue
		}
		key := target.PatNum + "|" + target.AptNum
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, target)
	}
	return out
}

func appointmentPatientTargets(targets []PatientTarget) []appointments.PatientTarget {
	out := make([]appointments.PatientTarget, 0, len(targets))
	for _, target := range normalizePatientTargets(targets) {
		out = append(out, appointments.PatientTarget{
			PatNum: target.PatNum,
			AptNum: target.AptNum,
		})
	}
	return out
}

func normalizePatNums(values []string) []string {
	out := make([]string, 0, len(values))
	seen := map[string]struct{}{}
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	return out
}

func targetsKey(targets []PatientTarget) string {
	targets = normalizePatientTargets(targets)
	parts := make([]string, 0, len(targets))
	for _, target := range targets {
		parts = append(parts, target.PatNum+"|"+target.AptNum)
	}
	return strings.Join(parts, ",")
}

func dedupeAppointmentsByPatNumOrdinal(rows []models.Appointment) []models.Appointment {
	uniqueRows := make([]models.Appointment, 0, len(rows))
	seen := map[string]struct{}{}

	for _, row := range rows {
		if row.PatNum == "" {
			uniqueRows = append(uniqueRows, row)
			continue
		}
		key := appointmentIdentityKey(row)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		uniqueRows = append(uniqueRows, row)
	}

	return uniqueRows
}

func appointmentIdentityKey(row models.Appointment) string {
	ordinal := strings.TrimSpace(row.Ordinal)
	if ordinal == "" {
		ordinal = "1"
	}
	return strings.Join([]string{
		strings.TrimSpace(row.PatNum),
		strings.TrimSpace(row.AptNum),
		strings.TrimSpace(row.InsSubNum),
		strings.TrimSpace(row.PlanNum),
		ordinal,
	}, "|")
}

func testingWithoutApptField(testing config.TestingConfig) config.TestingConfig {
	skip := true
	testing.SkipApptField = &skip
	return testing
}

func (m *Manager) nextRunID() string {
	seq := m.runSeq.Add(1)
	return fmt.Sprintf("run-%d-%d", time.Now().UTC().Unix(), seq)
}

func (m *Manager) markStarted(runID string, req TriggerRequest) {
	now := time.Now().UTC()

	m.stateMu.Lock()
	defer m.stateMu.Unlock()

	startedAt := now
	lastCompleted := m.state.LastCompletedAt
	m.state = RunState{
		Running:         true,
		RunID:           runID,
		Action:          req.Action,
		PatNum:          req.PatNum,
		PatNums:         append([]string(nil), req.PatNums...),
		PatientTargets:  append([]PatientTarget(nil), req.PatientTargets...),
		AddDays:         req.AddDays,
		RequestedBy:     req.RequestedBy,
		StartedAt:       &startedAt,
		LastCompletedAt: lastCompleted,
	}
}

func (m *Manager) markCompleted(err error) {
	now := time.Now().UTC()

	m.stateMu.Lock()
	defer m.stateMu.Unlock()

	lastCompleted := now
	m.state.Running = false
	m.state.RunID = ""
	m.state.StartedAt = nil
	m.state.LastCompletedAt = &lastCompleted
	if err != nil {
		m.state.LastError = err.Error()
		return
	}
	m.state.LastError = ""
}

func cloneRunState(state RunState) RunState {
	cloned := state
	cloned.PatNums = append([]string(nil), state.PatNums...)
	cloned.PatientTargets = append([]PatientTarget(nil), state.PatientTargets...)
	if state.StartedAt != nil {
		startedAt := *state.StartedAt
		cloned.StartedAt = &startedAt
	}
	if state.LastCompletedAt != nil {
		lastCompletedAt := *state.LastCompletedAt
		cloned.LastCompletedAt = &lastCompletedAt
	}
	return cloned
}

func collectPayerIDs(payers []models.Payer) []string {
	seen := make(map[string]struct{})
	all := make([]string, 0)
	for _, payer := range payers {
		for _, payerID := range payer.PayerIDs {
			key := strings.ToLower(strings.TrimSpace(payerID))
			if key == "" || key == "*" {
				continue
			}
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			all = append(all, payerID)
		}
	}
	return all
}

type payerAppointmentBuckets struct {
	Supported   map[string][]models.Appointment
	Unsupported map[string][]models.Appointment
}

func bucketAppointmentsByPayer(payers []models.Payer, rows []models.Appointment) payerAppointmentBuckets {
	// Detect catch-all payer (payerIDs contains "*") — receives all unsupported appointments.
	var catchAll *models.Payer
	byID := make(map[string]models.Payer)
	for _, payer := range payers {
		for _, payerID := range payer.PayerIDs {
			if strings.TrimSpace(payerID) == "*" {
				p := payer
				catchAll = &p
				continue
			}
			key := strings.ToLower(strings.TrimSpace(payerID))
			if key == "" {
				continue
			}
			byID[key] = payer
		}
	}

	grouped := make(map[string][]models.Appointment)
	unsupported := make(map[string][]models.Appointment)
	seen := make(map[string]map[string]struct{})
	for _, row := range rows {
		payerID := strings.TrimSpace(row.PayerID)
		if payerID == "" {
			unsupported[""] = append(unsupported[""], row)
			continue
		}

		payer, ok := byID[strings.ToLower(payerID)]
		if !ok {
			if catchAll != nil {
				if seen[catchAll.PayerURL] == nil {
					seen[catchAll.PayerURL] = make(map[string]struct{})
				}
				if row.PatNum != "" {
					key := appointmentIdentityKey(row)
					if _, exists := seen[catchAll.PayerURL][key]; exists {
						continue
					}
					seen[catchAll.PayerURL][key] = struct{}{}
				}
				grouped[catchAll.PayerURL] = append(grouped[catchAll.PayerURL], row)
			} else {
				unsupported[payerID] = append(unsupported[payerID], row)
			}
			continue
		}
		if seen[payer.PayerURL] == nil {
			seen[payer.PayerURL] = make(map[string]struct{})
		}
		if row.PatNum != "" {
			key := appointmentIdentityKey(row)
			if _, exists := seen[payer.PayerURL][key]; exists {
				continue
			}
			seen[payer.PayerURL][key] = struct{}{}
		}
		grouped[payer.PayerURL] = append(grouped[payer.PayerURL], row)
	}
	return payerAppointmentBuckets{
		Supported:   grouped,
		Unsupported: unsupported,
	}
}

func logUnsupportedPayerRows(action string, patNum string, addDays int, unsupported map[string][]models.Appointment) {
	for payerID, rows := range unsupported {
		label := payerID
		if label == "" {
			label = "<empty>"
		}
		if patNum != "" {
			log.Printf("unsupported payer bucket: action=%s patnum=%s payerId=%s appointments=%d", action, patNum, label, len(rows))
			continue
		}
		log.Printf("unsupported payer bucket: action=%s addDays=%d payerId=%s appointments=%d", action, addDays, label, len(rows))
	}
}

func logPayerBucketSummary(action string, patNum string, addDays int, payers []models.Payer, buckets payerAppointmentBuckets) {
	parts := make([]string, 0, len(payers)+1)
	for _, payer := range payers {
		count := len(buckets.Supported[payer.PayerURL])
		if count == 0 {
			continue
		}
		parts = append(parts, fmt.Sprintf("%s: %d patients", payer.PayerURL, count))
	}

	unsupportedCount := 0
	for _, rows := range buckets.Unsupported {
		unsupportedCount += len(rows)
	}
	parts = append(parts, fmt.Sprintf("unsupported: %d", unsupportedCount))

	if patNum != "" {
		log.Printf("payer bucket summary: action=%s patnum=%s %s", action, patNum, strings.Join(parts, ", "))
		return
	}
	log.Printf("payer bucket summary: action=%s addDays=%d %s", action, addDays, strings.Join(parts, ", "))
}

func firstWildcardPayer(payerList []models.Payer) (models.Payer, bool) {
	for _, payer := range payerList {
		for _, payerID := range payer.PayerIDs {
			if strings.TrimSpace(payerID) == "*" {
				return payer, true
			}
		}
	}
	return models.Payer{}, false
}

func fallbackEligibleAppointments(probeDir, payerURL string, appointments []models.Appointment, runErr error) []models.Appointment {
	runErrType := payers.ClassifyProbeError(runErr)
	runErrFallback := isFallbackProbeErrorType(runErrType)
	var eligible []models.Appointment
	for _, appt := range appointments {
		if probeArtifactExists(probeDir, payerURL, appt, "api_probe") || probeArtifactExists(probeDir, payerURL, appt, "probe") {
			continue
		}
		probeErr, err := payers.ReadProbeErrorForAppointment(probeDir, payerURL, appt)
		if err == nil {
			if isFallbackProbeErrorType(probeErr.ErrorType) {
				eligible = append(eligible, appt)
			}
			continue
		}
		if runErrFallback {
			eligible = append(eligible, appt)
		}
	}
	return eligible
}

func isFallbackProbeErrorType(errorType string) bool {
	switch strings.TrimSpace(errorType) {
	case payers.ProbeErrorPayer, payers.ProbeErrorSystem, payers.ProbeErrorUnknown:
		return true
	default:
		return false
	}
}

func probeArtifactExists(probeDir, payerURL string, appt models.Appointment, suffix string) bool {
	path := payers.ProbeFilePathForAppointment(probeDir, payerURL, appt, suffix)
	_, err := os.Stat(path)
	return err == nil
}

func fallbackReason(err error) string {
	return payers.ClassifyProbeError(err)
}

func replaceQueuedAppointmentsWithFallback(run QueuedRun, originalPayerURL, fallbackPayerURL, reason string, appointments []models.Appointment) QueuedRun {
	fallbackKeys := make(map[string]models.Appointment, len(appointments))
	for _, appt := range appointments {
		fallbackKeys[queuedAppointmentResultKey(appt)] = appt
	}
	var updated []QueuedAppointment
	for _, queued := range run.Appointments {
		key := queuedAppointmentResultKey(queued.Appointment)
		if strings.EqualFold(queued.PayerURL, originalPayerURL) {
			if _, shouldReplace := fallbackKeys[key]; shouldReplace {
				continue
			}
		}
		updated = append(updated, queued)
	}
	for _, appt := range appointments {
		updated = append(updated, QueuedAppointment{
			PayerURL:         fallbackPayerURL,
			Appointment:      appt,
			FallbackEligible: true,
			OriginalPayerURL: originalPayerURL,
			FallbackReason:   reason,
		})
	}
	run.Appointments = updated
	return run
}

func appendFallbackAppointments(existing []models.Appointment, fallbacks []models.Appointment) []models.Appointment {
	seen := make(map[string]struct{}, len(existing))
	for _, appt := range existing {
		seen[appointmentIdentityKey(appt)] = struct{}{}
	}
	for _, appt := range fallbacks {
		key := appointmentIdentityKey(appt)
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		existing = append(existing, appt)
	}
	return existing
}

// buildQueuedRun constructs a QueuedRun, preserving ReceivedAt and RetryCount
// from an existing queue file when this runID was previously pending.
func (m *Manager) buildQueuedRun(runID string, req TriggerRequest, appts []QueuedAppointment) QueuedRun {
	receivedAt := time.Now().UTC()
	retryCount := 0
	if existing, err := loadQueueFile(defaultQueueDir, runID); err == nil {
		receivedAt = existing.ReceivedAt
		retryCount = existing.RetryCount
	}
	return QueuedRun{
		RunID:              runID,
		Action:             req.Action,
		PatNum:             req.PatNum,
		PatNums:            req.PatNums,
		PatientTargets:     req.PatientTargets,
		AddDays:            req.AddDays,
		RequestedBy:        req.RequestedBy,
		ReceivedAt:         receivedAt,
		RetryCount:         retryCount,
		Appointments:       appts,
		SourceAppointments: req.Appointments,
		OfficeIdentity:     req.OfficeIdentity,
	}
}

func (m *Manager) writeQueueProbeErrors(runID string, remaining []QueuedAppointment, probeDir string) {
	type probeErrorFile struct {
		RunID        string              `json:"runId"`
		WrittenAt    time.Time           `json:"writtenAt"`
		Appointments []QueuedAppointment `json:"appointments"`
	}
	data, err := json.MarshalIndent(probeErrorFile{
		RunID:        runID,
		WrittenAt:    time.Now().UTC(),
		Appointments: remaining,
	}, "", "  ")
	if err != nil {
		return
	}
	if mkErr := os.MkdirAll(probeDir, 0o755); mkErr != nil {
		return
	}
	path := filepath.Join(probeDir, "probe_error_"+runID+".json")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		log.Printf("write probe error file failed runId=%s: %v", runID, err)
	} else {
		log.Printf("probe error file written runId=%s path=%s appointments=%d", runID, path, len(remaining))
	}

	for _, qa := range remaining {
		if _, err := payers.ReadProbeErrorForAppointment(probeDir, qa.PayerURL, qa.Appointment); err == nil {
			continue
		}
		payload := map[string]any{
			"recordedAt":  time.Now().UTC().Format(time.RFC3339),
			"appointment": qa.Appointment,
			"error":       "payer/site/system failure after retries",
			"errorType":   payers.ProbeErrorSystem,
		}
		data, err := json.MarshalIndent(payload, "", "  ")
		if err != nil {
			continue
		}
		path := payers.ProbeFilePathForAppointment(probeDir, qa.PayerURL, qa.Appointment, "probe_error")
		if err := os.WriteFile(path, data, 0o644); err != nil {
			log.Printf("write patient probe error failed runId=%s payerUrl=%s patNum=%s aptNum=%s: %v",
				runID, qa.PayerURL, qa.Appointment.PatNum, qa.Appointment.AptNum, err)
		}
	}
}

// StartQueueChecker runs the idle queue processor in a background goroutine.
// It checks every 30 seconds and processes one pending run per tick when idle.
func (m *Manager) StartQueueChecker(ctx context.Context) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.processNextQueuedRun(ctx)
		}
	}
}

func (m *Manager) processNextQueuedRun(ctx context.Context) {
	runs, err := loadPendingQueueFiles(defaultQueueDir)
	if err != nil {
		log.Printf("queue checker: load error: %v", err)
		return
	}
	if len(runs) == 0 {
		return
	}

	// Probed/postprocessing runs go first — they don't need the browser and
	// should complete before starting a new probing run.
	sortPendingRunsForProcessing(runs)

	now := time.Now().UTC()
	for _, run := range runs {
		if run.NextRetryAt != nil && run.NextRetryAt.After(now) {
			continue
		}
		if !m.runMu.TryLock() {
			return // another run already in progress
		}
		req := TriggerRequest{
			Action:         run.Action,
			PatNum:         run.PatNum,
			PatNums:        run.PatNums,
			PatientTargets: run.PatientTargets,
			AddDays:        run.AddDays,
			RequestedBy:    run.RequestedBy,
			Appointments:   run.SourceAppointments,
		}
		m.markStarted(run.RunID, req)
		go func(r QueuedRun) {
			defer m.runMu.Unlock()
			err := m.runQueuedItem(ctx, r)
			if err != nil {
				log.Printf("queued run failed: runId=%s action=%s err=%v", r.RunID, r.Action, err)
			} else {
				log.Printf("queued run completed: runId=%s action=%s", r.RunID, r.Action)
			}
			m.markCompleted(err)
		}(run)
		return // one run per tick
	}
}

func sortPendingRunsForProcessing(runs []QueuedRun) {
	sort.SliceStable(runs, func(i, j int) bool {
		iPriority := queuedRunPriority(runs[i])
		jPriority := queuedRunPriority(runs[j])
		if iPriority != jPriority {
			return iPriority < jPriority
		}
		return runs[i].ReceivedAt.Before(runs[j].ReceivedAt)
	})
}

func queuedRunPriority(run QueuedRun) int {
	switch run.Phase {
	case PhaseProbed, PhasePostprocessing:
		return 0
	}
	if run.Action == "run_now" {
		return 1
	}
	return 2
}

func (m *Manager) runQueuedItem(ctx context.Context, run QueuedRun) error {
	snapshot, err := m.prepareSnapshot(ctx)
	if err != nil {
		return err
	}
	officeCodes, err := m.ensureOfficeCodes(ctx, snapshot.ScraperConfig)
	if err != nil {
		return err
	}

	// Crash recovery: Phase 1 already completed — go straight to Phase 2.
	switch run.Phase {
	case PhaseProbed, PhasePostprocessing:
		m.runPostProcess(ctx, run, snapshot, officeCodes)
		return nil
	}

	// Phase 1 (probing) — either a retry with stored appointments or a fresh run.
	if len(run.Appointments) > 0 {
		return m.replayStoredAppointments(ctx, run, snapshot, officeCodes)
	}

	req := TriggerRequest{
		Action:         run.Action,
		PatNum:         run.PatNum,
		PatNums:        run.PatNums,
		PatientTargets: run.PatientTargets,
		AddDays:        run.AddDays,
		RequestedBy:    run.RequestedBy,
		Appointments:   run.SourceAppointments,
		OfficeIdentity: run.OfficeIdentity,
	}
	switch req.Action {
	case "run_now":
		return m.runNow(ctx, run.RunID, req, snapshot, officeCodes)
	case "run_all":
		return m.runAll(ctx, run.RunID, req, snapshot, officeCodes)
	default:
		return fmt.Errorf("unsupported action %q in queued run", req.Action)
	}
}

// replayStoredAppointments re-runs Phase 1 for the appointments stored in a retry queue file.
func (m *Manager) replayStoredAppointments(ctx context.Context, run QueuedRun, snapshot *cache.WorkSnapshot, officeCodes []string) error {
	run.Phase = PhaseProbing
	if err := persistQueueFile(defaultQueueDir, run); err != nil {
		log.Printf("queue phase update failed runId=%s: %v", run.RunID, err)
	}

	grouped := make(map[string][]models.Appointment)
	for _, qa := range run.Appointments {
		if qa.ProbeComplete {
			continue
		}
		grouped[qa.PayerURL] = append(grouped[qa.PayerURL], qa.Appointment)
	}

	pdfQueue := &deferredPDFQueue{}
	for _, payer := range snapshot.Payers {
		appts := grouped[payer.PayerURL]
		if len(appts) == 0 {
			continue
		}
		if err := m.processPayerAppointments(ctx, snapshot, payer, officeCodes, appts, pdfQueue, run.OfficeIdentity); err != nil {
			log.Printf("replay payer failed: payerUrl=%s runId=%s: %v", payer.PayerURL, run.RunID, err)
		}
	}
	m.drainDeferredPDFs(ctx, snapshot, pdfQueue)
	m.finalizeProbing(ctx, run, snapshot, officeCodes)
	return nil
}

// finalizeProbing checks probe files after Phase 1. If all appointments are probed (or retries
// exhausted), advances immediately to Phase 2 (postprocess). Otherwise schedules a Phase 1 retry.
func (m *Manager) finalizeProbing(ctx context.Context, run QueuedRun, snapshot *cache.WorkSnapshot, officeCodes []string) {
	if len(run.Appointments) == 0 {
		_ = removeQueueFile(defaultQueueDir, run.RunID)
		return
	}

	probeDir := m.currentProbeOutputDir()
	done, remaining := checkProbesDone(probeDir, run.Appointments)

	// Stamp ProbeComplete on appointments whose probe file exists.
	doneSet := make(map[string]bool, len(done))
	for _, a := range done {
		doneSet[queuedAppointmentKey(a)] = true
	}
	for i := range run.Appointments {
		a := &run.Appointments[i]
		if doneSet[queuedAppointmentKey(*a)] {
			a.ProbeComplete = true
		}
	}

	if len(remaining) > 0 && run.RetryCount >= maxQueueRetries {
		log.Printf("probe retries exhausted: runId=%s remaining=%d advancing remaining to postprocess as errors", run.RunID, len(remaining))
		m.writeQueueProbeErrors(run.RunID, remaining, probeDir)
		for i := range run.Appointments {
			a := &run.Appointments[i]
			for _, r := range remaining {
				if a.PayerURL == r.PayerURL &&
					queuedAppointmentResultKey(a.Appointment) == queuedAppointmentResultKey(r.Appointment) {
					a.ProbeComplete = true
					break
				}
			}
		}
		remaining = nil
	}

	ready, pending := partitionProbeCompleteGroups(run.Appointments)
	if len(ready) > 0 {
		readyRun := run
		readyRun.Appointments = ready
		readyRun.Phase = PhaseProbed
		readyRun.NextRetryAt = nil
		log.Printf("partial postprocess ready: runId=%s ready=%d pending=%d", run.RunID, len(ready), len(pending))
		m.runPostProcessPartial(ctx, readyRun, snapshot, officeCodes)
	}

	if len(pending) == 0 {
		if err := removeQueueFile(defaultQueueDir, run.RunID); err != nil {
			log.Printf("remove queue file failed runId=%s: %v", run.RunID, err)
		}
		return
	}

	if len(remaining) == 0 && len(ready) == 0 {
		// No new completed groups and no remaining probes means everything was already
		// pending on another ordinal; keep the queue record as-is.
		return
	}

	retryInterval := normalRetryInterval
	pendingRemaining := incompleteProbeAppointments(pending)
	if detectPayerDown(pendingRemaining) {
		retryInterval = payerDownRetryInterval
		log.Printf("payer down detected: runId=%s remaining=%d retry in %s", run.RunID, len(pendingRemaining), retryInterval)
	} else {
		log.Printf("probe retry scheduled: runId=%s remaining=%d retry in 5m", run.RunID, len(pendingRemaining))
	}
	nextRetry := time.Now().UTC().Add(retryInterval)
	updated := run
	updated.Appointments = pending
	updated.RetryCount = run.RetryCount + 1
	updated.NextRetryAt = &nextRetry
	updated.Phase = PhaseProbing
	if err := persistQueueFile(defaultQueueDir, updated); err != nil {
		log.Printf("update queue file failed runId=%s: %v", run.RunID, err)
	}
}

// runPostProcess runs Phase 2 (eligibility + advanced.json) for all stored appointments.
// The run lock is already held by the caller — new triggers queue while this runs.
func (m *Manager) runPostProcess(ctx context.Context, run QueuedRun, snapshot *cache.WorkSnapshot, officeCodes []string) {
	run.Phase = PhasePostprocessing
	if err := persistQueueFile(defaultQueueDir, run); err != nil {
		log.Printf("phase postprocessing update failed runId=%s: %v", run.RunID, err)
	}
	m.postProcessQueuedRun(ctx, run, snapshot, officeCodes, true)

	if err := removeQueueFile(defaultQueueDir, run.RunID); err != nil {
		log.Printf("remove queue file failed runId=%s: %v", run.RunID, err)
	}
	log.Printf("postprocess done")
}

func (m *Manager) runPostProcessPartial(ctx context.Context, run QueuedRun, snapshot *cache.WorkSnapshot, officeCodes []string) {
	run.Phase = PhasePostprocessing
	m.postProcessQueuedRun(ctx, run, snapshot, officeCodes, false)
	log.Printf("partial postprocess done runId=%s appointments=%d", run.RunID, len(run.Appointments))
}

func (m *Manager) postProcessQueuedRun(ctx context.Context, run QueuedRun, snapshot *cache.WorkSnapshot, officeCodes []string, persistResults bool) {
	grouped := make(map[string][]models.Appointment)
	for _, qa := range run.Appointments {
		if !qa.ProbeComplete {
			log.Printf("postprocess skipping incomplete probe payerUrl=%s patNum=%s aptNum=%s",
				qa.PayerURL, qa.Appointment.PatNum, qa.Appointment.AptNum)
			continue
		}
		grouped[qa.PayerURL] = append(grouped[qa.PayerURL], qa.Appointment)
	}

	pdfQueue := &deferredPDFQueue{}
	for _, payer := range snapshot.Payers {
		appts := grouped[payer.PayerURL]
		if len(appts) == 0 {
			continue
		}
		summary, err := m.processPayerAppointmentsPostProcess(ctx, snapshot, payer, officeCodes, appts, pdfQueue, run.OfficeIdentity)
		run = recordQueueResults(run, summary.Results)
		if err != nil {
			log.Printf("postprocess payer failed: payerUrl=%s runId=%s: %v", payer.PayerURL, run.RunID, err)
		}
	}
	if persistResults {
		if err := persistQueueFile(defaultQueueDir, run); err != nil {
			log.Printf("queue result update failed runId=%s: %v", run.RunID, err)
		}
	}
	m.writeCompletedAppointmentFields(snapshot, run)
	m.notifyCompletedRunNowPatients(ctx, snapshot, run)
	m.drainDeferredPDFs(ctx, snapshot, pdfQueue)
}

// processPayerAppointmentsPostProcess runs Phase 2 only — no browser, reads existing probe files.
func (m *Manager) processPayerAppointmentsPostProcess(ctx context.Context, snapshot *cache.WorkSnapshot, payer models.Payer, officeCodes []string, selected []models.Appointment, pdfQueue *deferredPDFQueue, identity odetrans.OfficeIdentity) (payers.RunSummary, error) {
	if m.cfg.Local.ShouldSkipPayer(payer.PayerURL) {
		log.Printf("skipping payer (local override): payerUrl=%s", payer.PayerURL)
		return payers.RunSummary{}, nil
	}
	sessionInput, err := m.buildPayerSessionInput(ctx, snapshot, payer, officeCodes, selected, identity)
	if err != nil {
		return payers.RunSummary{}, err
	}
	sessionInput.SkipProbing = true
	if err := m.attachPayerRuntimeState(snapshot, sessionInput); err != nil {
		return payers.RunSummary{}, err
	}
	return m.runPayerSession(ctx, snapshot, payer, sessionInput, pdfQueue)
}

func recordQueueResults(run QueuedRun, results []payers.PatientResult) QueuedRun {
	byKey := make(map[string]string, len(results))
	byPatOrdinal := make(map[string]string, len(results))
	for _, result := range results {
		key := appointmentResultKey(result.PatNum, result.AptNum, result.Ordinal)
		value := resultwriter.AppointmentOrdinalFieldValue(result.Status, result.Ordinal)
		if key != "" {
			byKey[key] = value
		}
		byPatOrdinal[strings.TrimSpace(result.PatNum)+"|"+normalizedOrdinal(result.Ordinal)] = value
	}
	for i := range run.Appointments {
		appointment := run.Appointments[i].Appointment
		key := queuedAppointmentResultKey(appointment)
		if value, ok := byKey[key]; ok {
			run.Appointments[i].ResultComplete = true
			run.Appointments[i].ResultValue = value
			continue
		}
		fallbackKey := strings.TrimSpace(appointment.PatNum) + "|" + normalizedOrdinal(appointment.Ordinal)
		if strings.TrimSpace(appointment.AptNum) == "" {
			if value, ok := byPatOrdinal[fallbackKey]; ok {
				run.Appointments[i].ResultComplete = true
				run.Appointments[i].ResultValue = value
			}
		}
	}
	return run
}

func (m *Manager) writeCompletedAppointmentFields(snapshot *cache.WorkSnapshot, run QueuedRun) {
	if snapshot == nil || snapshot.ScraperConfig == nil {
		return
	}
	writer, err := resultwriter.New(m.cfg.Testing, snapshot.ScraperConfig.APIs)
	if err != nil {
		log.Printf("resultwriter unavailable for coordinated appt field: %v", err)
		return
	}

	groups := make(map[string][]QueuedAppointment)
	for _, queued := range run.Appointments {
		key := appointmentGroupKey(queued.Appointment)
		if key == "" {
			continue
		}
		groups[key] = append(groups[key], queued)
	}

	for _, group := range groups {
		if len(group) == 0 || !appointmentGroupResultsComplete(group) {
			continue
		}
		sort.SliceStable(group, func(i, j int) bool {
			return ordinalSortValue(group[i].Appointment.Ordinal) < ordinalSortValue(group[j].Appointment.Ordinal)
		})
		parts := make([]string, 0, len(group))
		for _, queued := range group {
			parts = append(parts, queued.ResultValue)
		}
		writer.ApplyAppointmentFieldValue(group[0].Appointment, snapshot.OfficeKey, strings.Join(parts, ", "))
	}
}

func appointmentGroupResultsComplete(group []QueuedAppointment) bool {
	seenOrdinals := map[string]struct{}{}
	for _, queued := range group {
		ordinal := normalizedOrdinal(queued.Appointment.Ordinal)
		if _, ok := seenOrdinals[ordinal]; ok {
			continue
		}
		if !queued.ResultComplete || strings.TrimSpace(queued.ResultValue) == "" {
			return false
		}
		seenOrdinals[ordinal] = struct{}{}
	}
	return len(seenOrdinals) > 0
}

func partitionProbeCompleteGroups(appointments []QueuedAppointment) (ready, pending []QueuedAppointment) {
	groups := make(map[string][]QueuedAppointment)
	order := make([]string, 0)
	for _, queued := range appointments {
		key := appointmentGroupKey(queued.Appointment)
		if key == "" {
			key = queued.PayerURL + "|" + queued.Appointment.PatNum + "|" + queued.Appointment.AptNum
		}
		if _, exists := groups[key]; !exists {
			order = append(order, key)
		}
		groups[key] = append(groups[key], queued)
	}
	for _, key := range order {
		group := groups[key]
		if appointmentGroupProbesComplete(group) {
			ready = append(ready, group...)
		} else {
			pending = append(pending, group...)
		}
	}
	return ready, pending
}

func appointmentGroupProbesComplete(group []QueuedAppointment) bool {
	if len(group) == 0 {
		return false
	}
	for _, queued := range group {
		if !queued.ProbeComplete {
			return false
		}
	}
	return true
}

func incompleteProbeAppointments(appointments []QueuedAppointment) []QueuedAppointment {
	var incomplete []QueuedAppointment
	for _, queued := range appointments {
		if !queued.ProbeComplete {
			incomplete = append(incomplete, queued)
		}
	}
	return incomplete
}

func queuedAppointmentKey(queued QueuedAppointment) string {
	return queued.PayerURL + "|" + queuedAppointmentResultKey(queued.Appointment)
}

func appointmentResultKey(patNum, aptNum, ordinal string) string {
	patNum = strings.TrimSpace(patNum)
	aptNum = strings.TrimSpace(aptNum)
	if patNum == "" {
		return ""
	}
	return patNum + "|" + workItemAppointmentToken(aptNum, "", "", ordinal) + "|" + normalizedOrdinal(ordinal)
}

func appointmentGroupKey(appointment models.Appointment) string {
	patNum := strings.TrimSpace(appointment.PatNum)
	aptNum := strings.TrimSpace(appointment.AptNum)
	if patNum == "" || aptNum == "" {
		return ""
	}
	return patNum + "|" + aptNum
}

func queuedAppointmentResultKey(appointment models.Appointment) string {
	patNum := strings.TrimSpace(appointment.PatNum)
	if patNum == "" {
		return ""
	}
	return patNum + "|" + workItemAppointmentToken(appointment.AptNum, appointment.InsSubNum, appointment.PlanNum, appointment.Ordinal) + "|" + normalizedOrdinal(appointment.Ordinal)
}

func workItemAppointmentToken(aptNum, insSubNum, planNum, ordinal string) string {
	aptNum = strings.TrimSpace(aptNum)
	if aptNum != "" {
		return aptNum
	}
	insSubNum = strings.TrimSpace(insSubNum)
	planNum = strings.TrimSpace(planNum)
	token := "noappt"
	if insSubNum != "" || planNum != "" {
		token += ":" + insSubNum + ":" + planNum
	} else {
		token += ":" + normalizedOrdinal(ordinal)
	}
	return token
}

func normalizedOrdinal(ordinal string) string {
	ordinal = strings.TrimSpace(ordinal)
	if ordinal == "" {
		return "1"
	}
	return ordinal
}

func ordinalSortValue(ordinal string) int {
	switch normalizedOrdinal(ordinal) {
	case "1":
		return 1
	case "2":
		return 2
	default:
		return 100
	}
}
