package triggerapi

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"insurance-benefit-agent-go/internal/config"
	"insurance-benefit-agent-go/internal/jobmgr"
)

type stubRunner struct {
	lastRequest jobmgr.TriggerRequest
	runID       string
	err         error
	state       jobmgr.RunState
}

func (s *stubRunner) Trigger(ctx context.Context, req jobmgr.TriggerRequest) (string, error) {
	s.lastRequest = req
	return s.runID, s.err
}

func (s *stubRunner) Status() jobmgr.RunState {
	return s.state
}

func TestHandleTriggersRunNowAccepted(t *testing.T) {
	runner := &stubRunner{runID: "run-1"}
	server := New(config.APIConfig{BearerToken: "secret"}, "OFFICE-1", runner, nil)
	server.rootCtx = context.Background()

	body := []byte(`{"action":"run_now","data":[{"patnum":1235}],"requestedBy":"SchedulerService"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/triggers", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()

	server.handleTriggers(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("expected %d, got %d", http.StatusAccepted, rec.Code)
	}

	var got triggerResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if !got.Accepted || got.Status != "queued" || got.Action != "run_now" || got.RunID != "run-1" || got.OfficeKey != "OFFICE-1" {
		t.Fatalf("unexpected response: %+v", got)
	}
	if runner.lastRequest.Action != "run_now" || runner.lastRequest.PatNum != "1235" || len(runner.lastRequest.PatNums) != 1 || runner.lastRequest.PatNums[0] != "1235" {
		t.Fatalf("unexpected trigger request: %+v", runner.lastRequest)
	}
	if len(runner.lastRequest.PatientTargets) != 1 || runner.lastRequest.PatientTargets[0].PatNum != "1235" {
		t.Fatalf("unexpected patient targets: %+v", runner.lastRequest.PatientTargets)
	}
}

func TestHandleTriggersRunNowAcceptsDataTargets(t *testing.T) {
	runner := &stubRunner{runID: "run-1"}
	server := New(config.APIConfig{BearerToken: "secret"}, "OFFICE-1", runner, nil)
	server.rootCtx = context.Background()

	body := []byte(`{"action":"run_now","data":[{"patnum":35,"aptnum":4567},{"patnum":110},{"patnum":24}],"requestedBy":"Campaign"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/triggers", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()

	server.handleTriggers(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("expected %d, got %d", http.StatusAccepted, rec.Code)
	}
	if runner.lastRequest.PatNum != "35,110,24" {
		t.Fatalf("PatNum=%q", runner.lastRequest.PatNum)
	}
	if runner.lastRequest.PatientTargets[0].AptNum != "4567" {
		t.Fatalf("targets=%+v", runner.lastRequest.PatientTargets)
	}
	want := []string{"35", "110", "24"}
	if len(runner.lastRequest.PatNums) != len(want) {
		t.Fatalf("PatNums=%v, want %v", runner.lastRequest.PatNums, want)
	}
	for i := range want {
		if runner.lastRequest.PatNums[i] != want[i] {
			t.Fatalf("PatNums=%v, want %v", runner.lastRequest.PatNums, want)
		}
	}
}

func TestHandleTriggersRunAllToday(t *testing.T) {
	runner := &stubRunner{}
	server := New(config.APIConfig{BearerToken: "secret"}, "OFFICE-1", runner, nil)
	server.rootCtx = context.Background()

	body := []byte(`{"action":"run_all","addDays":0}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/triggers", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()

	server.handleTriggers(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("expected %d, got %d", http.StatusAccepted, rec.Code)
	}
	if runner.lastRequest.Action != "run_all" || runner.lastRequest.AddDays != 0 {
		t.Fatalf("unexpected trigger request: %+v", runner.lastRequest)
	}
}

func TestHandleTriggersRunAllInvalid(t *testing.T) {
	runner := &stubRunner{}
	server := New(config.APIConfig{BearerToken: "secret"}, "OFFICE-1", runner, nil)
	server.rootCtx = context.Background()

	body := []byte(`{"action":"run_all","addDays":-1}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/triggers", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()

	server.handleTriggers(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected %d, got %d", http.StatusBadRequest, rec.Code)
	}
}

func TestHandleTriggersBusy(t *testing.T) {
	runner := &stubRunner{err: jobmgr.ErrRunInProgress}
	server := New(config.APIConfig{BearerToken: "secret"}, "OFFICE-1", runner, nil)
	server.rootCtx = context.Background()

	body := []byte(`{"action":"run_all","addDays":1}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/triggers", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()

	server.handleTriggers(rec, req)

	if rec.Code != http.StatusConflict {
		t.Fatalf("expected %d, got %d", http.StatusConflict, rec.Code)
	}
}

func TestHandleTriggersRunNowWarnsWhenRunAllActive(t *testing.T) {
	runner := &stubRunner{
		runID: "run-urgent",
		state: jobmgr.RunState{
			Running: true,
			RunID:   "run-batch",
			Action:  "run_all",
			AddDays: 0,
		},
	}
	server := New(config.APIConfig{BearerToken: "secret"}, "OFFICE-1", runner, nil)
	server.rootCtx = context.Background()

	body := []byte(`{"action":"run_now","data":[{"patnum":1235}],"requestedBy":"PatInfo"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/triggers", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()

	server.handleTriggers(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("expected %d, got %d", http.StatusAccepted, rec.Code)
	}
	var got triggerResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	want := "Queued. A run_all batch is currently running, so this run_now may be delayed."
	if got.Message != want {
		t.Fatalf("message = %q, want %q", got.Message, want)
	}
}

func TestHandleStatus(t *testing.T) {
	runner := &stubRunner{state: jobmgr.RunState{Running: true, Action: "run_all", AddDays: 1}}
	server := New(config.APIConfig{BearerToken: "secret"}, "OFFICE-1", runner, nil)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/status", nil)
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()

	server.handleStatus(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected %d, got %d", http.StatusOK, rec.Code)
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if got["officeKey"] != "OFFICE-1" {
		t.Fatalf("expected officeKey OFFICE-1, got %#v", got["officeKey"])
	}
	if got["version"] == nil {
		t.Fatalf("expected version in status response")
	}
}

func TestHandleVersion(t *testing.T) {
	runner := &stubRunner{}
	server := New(config.APIConfig{BearerToken: "secret"}, "OFFICE-1", runner, nil)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/version", nil)
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()

	server.handleVersion(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected %d, got %d", http.StatusOK, rec.Code)
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if got["officeKey"] != "OFFICE-1" {
		t.Fatalf("expected officeKey OFFICE-1, got %#v", got["officeKey"])
	}
	if got["version"] == nil {
		t.Fatalf("expected version response")
	}
}

func TestHandleUpdateCheckUnavailable(t *testing.T) {
	runner := &stubRunner{}
	server := New(config.APIConfig{BearerToken: "secret"}, "OFFICE-1", runner, nil)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/update/check", nil)
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()

	server.handleUpdateCheck(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected %d, got %d", http.StatusOK, rec.Code)
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if got["enabled"] != false {
		t.Fatalf("expected disabled updater response, got %#v", got)
	}
}
