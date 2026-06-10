package triggerapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"insurance-benefit-agent-go/internal/config"
	"insurance-benefit-agent-go/internal/jobmgr"
	"insurance-benefit-agent-go/internal/models"
	"insurance-benefit-agent-go/internal/odetrans"
	"insurance-benefit-agent-go/internal/updater"
	"insurance-benefit-agent-go/internal/version"
)

type Runner interface {
	Trigger(ctx context.Context, req jobmgr.TriggerRequest) (string, error)
	Status() jobmgr.RunState
}

type Server struct {
	cfg       config.APIConfig
	officeKey string
	manager   Runner
	updater   *updater.Service
	server    *http.Server
	rootCtx   context.Context
	exitFunc  func(int)
}

type triggerRequest struct {
	Action       string                    `json:"action"`
	PatNum       flexiblePatNums           `json:"patnum"`
	Data         []triggerPatientTarget    `json:"data"`
	AddDays      int                       `json:"addDays"`
	RequestedBy  string                    `json:"requestedBy"`
	Provider     odetrans.ProviderIdentity `json:"provider"`
	Practice     odetrans.PracticeIdentity `json:"practice"`
	Appointments []models.Appointment      `json:"appointments"`
}

type triggerPatientTarget struct {
	PatNum flexibleScalar `json:"patnum"`
	AptNum flexibleScalar `json:"aptnum"`
}

type flexibleScalar string

func (s *flexibleScalar) UnmarshalJSON(data []byte) error {
	var text string
	if err := json.Unmarshal(data, &text); err == nil {
		*s = flexibleScalar(strings.TrimSpace(text))
		return nil
	}
	var number float64
	if err := json.Unmarshal(data, &number); err == nil {
		*s = flexibleScalar(fmt.Sprintf("%.0f", number))
		return nil
	}
	if string(data) == "null" {
		*s = ""
		return nil
	}
	return fmt.Errorf("value must be a string or number")
}

type flexiblePatNums []string

func (p *flexiblePatNums) UnmarshalJSON(data []byte) error {
	var values []string
	if err := json.Unmarshal(data, &values); err == nil {
		*p = normalizePatNums(values)
		return nil
	}
	var numbers []float64
	if err := json.Unmarshal(data, &numbers); err == nil {
		for _, n := range numbers {
			values = append(values, fmt.Sprintf("%.0f", n))
		}
		*p = normalizePatNums(values)
		return nil
	}
	var text string
	if err := json.Unmarshal(data, &text); err == nil {
		*p = normalizePatNums([]string{text})
		return nil
	}
	var number float64
	if err := json.Unmarshal(data, &number); err == nil {
		*p = normalizePatNums([]string{fmt.Sprintf("%.0f", number)})
		return nil
	}
	return fmt.Errorf("patnum must be a string, number, or array")
}

type triggerResponse struct {
	Accepted  bool   `json:"accepted"`
	RunID     string `json:"runId,omitempty"`
	Status    string `json:"status"`
	Action    string `json:"action,omitempty"`
	OfficeKey string `json:"officeKey"`
	Message   string `json:"message,omitempty"`
}

func New(cfg config.APIConfig, officeKey string, manager Runner, updateSvc *updater.Service) *Server {
	s := &Server{
		cfg:       cfg,
		officeKey: officeKey,
		manager:   manager,
		updater:   updateSvc,
		exitFunc:  os.Exit,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", s.handleHealthz)
	mux.HandleFunc("/api/v1/status", s.handleStatus)
	mux.HandleFunc("/api/v1/triggers", s.handleTriggers)
	mux.HandleFunc("/api/v1/version", s.handleVersion)
	mux.HandleFunc("/api/v1/update/check", s.handleUpdateCheck)
	mux.HandleFunc("/api/v1/update/apply", s.handleUpdateApply)

	s.server = &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           mux,
		ReadHeaderTimeout: time.Duration(cfg.ReadTimeoutMS) * time.Millisecond,
	}
	return s
}

func (s *Server) Run(ctx context.Context) error {
	s.rootCtx = ctx

	shutdownDone := make(chan struct{})
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), time.Duration(s.cfg.ShutdownTimeoutMS)*time.Millisecond)
		defer cancel()
		if err := s.server.Shutdown(shutdownCtx); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("trigger api shutdown failed: %v", err)
		}
		close(shutdownDone)
	}()

	err := s.server.ListenAndServe()
	if errors.Is(err, http.ErrServerClosed) {
		<-shutdownDone
		return nil
	}
	return err
}

func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "officeKey": s.officeKey})
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}
	if !s.authorized(r) {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "unauthorized"})
		return
	}
	state := s.manager.Status()
	writeJSON(w, http.StatusOK, map[string]any{
		"officeKey":       s.officeKey,
		"version":         version.Get(),
		"running":         state.Running,
		"runId":           state.RunID,
		"action":          state.Action,
		"patnum":          state.PatNum,
		"patnums":         state.PatNums,
		"addDays":         state.AddDays,
		"requestedBy":     state.RequestedBy,
		"startedAt":       state.StartedAt,
		"lastCompletedAt": state.LastCompletedAt,
		"lastError":       state.LastError,
	})
}

func (s *Server) handleVersion(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}
	if !s.authorized(r) {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "unauthorized"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"officeKey": s.officeKey,
		"version":   version.Get(),
	})
}

func (s *Server) handleUpdateCheck(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}
	if !s.authorized(r) {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "unauthorized"})
		return
	}
	if s.updater == nil {
		writeJSON(w, http.StatusOK, map[string]any{"enabled": false, "reason": "updater unavailable"})
		return
	}
	writeJSON(w, http.StatusOK, s.updater.Check())
}

func (s *Server) handleUpdateApply(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}
	if !s.authorized(r) {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "unauthorized"})
		return
	}
	if s.updater == nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "updater unavailable"})
		return
	}
	state := s.manager.Status()
	if state.Running {
		writeJSON(w, http.StatusConflict, map[string]any{"error": "agent is busy", "runId": state.RunID})
		return
	}
	result, err := s.updater.Apply()
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error(), "check": result.Check})
		return
	}
	writeJSON(w, http.StatusAccepted, result)
	if result.Started {
		go func() {
			time.Sleep(500 * time.Millisecond)
			log.Printf("update apply accepted; exiting for updater")
			s.exitFunc(0)
		}()
	}
}

func (s *Server) handleTriggers(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}
	if !s.authorized(r) {
		log.Printf("trigger api unauthorized: method=%s path=%s remote=%s", r.Method, r.URL.Path, r.RemoteAddr)
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "unauthorized"})
		return
	}

	var body triggerRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid JSON body"})
		return
	}
	body.Action = strings.TrimSpace(strings.ToLower(body.Action))
	body.PatNum = normalizePatNums(body.PatNum)
	targets := normalizeTriggerTargets(body.Data, body.PatNum)
	patNums := patNumsFromTargets(targets)
	patNumText := strings.Join(patNums, ",")
	log.Printf("trigger api request: method=%s path=%s remote=%s officeKey=%s action=%s patnum=%s addDays=%d requestedBy=%s",
		r.Method, r.URL.Path, r.RemoteAddr, s.officeKey, body.Action, patNumText, body.AddDays, strings.TrimSpace(body.RequestedBy))
	if err := validateTriggerBody(body); err != nil {
		log.Printf("trigger api rejected: officeKey=%s action=%s patnum=%s addDays=%d err=%v",
			s.officeKey, body.Action, patNumText, body.AddDays, err)
		writeJSON(w, http.StatusBadRequest, triggerResponse{
			Accepted:  false,
			Status:    "invalid_request",
			Action:    body.Action,
			OfficeKey: s.officeKey,
			Message:   err.Error(),
		})
		return
	}

	activeState := s.manager.Status()
	runID, err := s.manager.Trigger(s.rootCtx, jobmgr.TriggerRequest{
		Action:         body.Action,
		PatNum:         patNumText,
		PatNums:        patNums,
		PatientTargets: targets,
		AddDays:        body.AddDays,
		RequestedBy:    firstNonEmpty(body.RequestedBy, "api"),
		Appointments:   body.Appointments,
		OfficeIdentity: odetrans.OfficeIdentity{
			Provider: body.Provider,
			Practice: body.Practice,
		},
	})
	if err != nil {
		if errors.Is(err, jobmgr.ErrRunInProgress) {
			log.Printf("trigger api busy: officeKey=%s action=%s patnum=%s addDays=%d",
				s.officeKey, body.Action, body.PatNum, body.AddDays)
			writeJSON(w, http.StatusConflict, triggerResponse{
				Accepted:  false,
				Status:    "busy",
				Action:    strings.TrimSpace(strings.ToLower(body.Action)),
				OfficeKey: s.officeKey,
				Message:   err.Error(),
			})
			return
		}
		log.Printf("trigger api invalid: officeKey=%s action=%s patnum=%s addDays=%d err=%v",
			s.officeKey, body.Action, body.PatNum, body.AddDays, err)
		writeJSON(w, http.StatusBadRequest, triggerResponse{
			Accepted:  false,
			Status:    "invalid_request",
			Action:    strings.TrimSpace(strings.ToLower(body.Action)),
			OfficeKey: s.officeKey,
			Message:   err.Error(),
		})
		return
	}

	message := triggerAcceptedMessage(body.Action, activeState)
	if message != "trigger accepted" {
		log.Printf("trigger api accepted with delay notice: officeKey=%s runId=%s action=%s patnum=%s addDays=%d activeRunId=%s activeAction=%s message=%q",
			s.officeKey, runID, body.Action, body.PatNum, body.AddDays, activeState.RunID, activeState.Action, message)
	}
	writeJSON(w, http.StatusAccepted, triggerResponse{
		Accepted:  true,
		RunID:     runID,
		Status:    "queued",
		Action:    strings.TrimSpace(strings.ToLower(body.Action)),
		OfficeKey: s.officeKey,
		Message:   message,
	})
}

func triggerAcceptedMessage(action string, active jobmgr.RunState) string {
	action = strings.TrimSpace(strings.ToLower(action))
	if !active.Running {
		return "trigger accepted"
	}
	switch strings.TrimSpace(strings.ToLower(active.Action)) {
	case "run_all":
		if action == "run_now" {
			return "Queued. A run_all batch is currently running, so this run_now may be delayed."
		}
		return "Queued. A run_all batch is currently running."
	case "run_now":
		return "Queued. Another run_now is currently running."
	default:
		return "Queued. Another agent run is currently active."
	}
}

func (s *Server) authorized(r *http.Request) bool {
	token := strings.TrimSpace(s.cfg.BearerToken)
	if token == "" {
		return false
	}
	authHeader := strings.TrimSpace(r.Header.Get("Authorization"))
	if strings.HasPrefix(strings.ToLower(authHeader), "bearer ") {
		return subtleConstantTimeCompare(strings.TrimSpace(authHeader[7:]), token)
	}
	return subtleConstantTimeCompare(strings.TrimSpace(r.Header.Get("X-Agent-Trigger-Token")), token)
}

func subtleConstantTimeCompare(left string, right string) bool {
	if len(left) != len(right) {
		return false
	}
	var diff byte
	for i := 0; i < len(left); i++ {
		diff |= left[i] ^ right[i]
	}
	return diff == 0
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
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

func normalizeTriggerTargets(data []triggerPatientTarget, legacyPatNums []string) []jobmgr.PatientTarget {
	var targets []jobmgr.PatientTarget
	for _, item := range data {
		targets = append(targets, jobmgr.PatientTarget{
			PatNum: string(item.PatNum),
			AptNum: string(item.AptNum),
		})
	}
	if len(targets) == 0 {
		for _, patNum := range legacyPatNums {
			targets = append(targets, jobmgr.PatientTarget{PatNum: patNum})
		}
	}
	return normalizeJobTargets(targets)
}

func normalizeJobTargets(targets []jobmgr.PatientTarget) []jobmgr.PatientTarget {
	out := make([]jobmgr.PatientTarget, 0, len(targets))
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

func patNumsFromTargets(targets []jobmgr.PatientTarget) []string {
	values := make([]string, 0, len(targets))
	for _, target := range targets {
		values = append(values, target.PatNum)
	}
	return normalizePatNums(values)
}

func validRequestedBy(value string) bool {
	switch strings.TrimSpace(value) {
	case "SchedulerService", "Campaign", "PatInfo":
		return true
	default:
		return false
	}
}

func validateTriggerBody(body triggerRequest) error {
	switch body.Action {
	case "run_now":
		if len(normalizeTriggerTargets(body.Data, body.PatNum)) == 0 && len(body.Appointments) == 0 {
			return fmt.Errorf("data is required when action=run_now")
		}
		if !validRequestedBy(body.RequestedBy) {
			return fmt.Errorf("requestedBy must be one of SchedulerService, Campaign, PatInfo when action=run_now")
		}
		return nil
	case "run_all":
		if body.AddDays < 0 {
			return fmt.Errorf("addDays must be >= 0 when action=run_all")
		}
		return nil
	default:
		return fmt.Errorf("unsupported action %q", body.Action)
	}
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	raw, err := json.Marshal(payload)
	if err != nil {
		http.Error(w, fmt.Sprintf(`{"error":"marshal response: %v"}`, err), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(raw)
	_, _ = w.Write([]byte("\n"))
}
