package jobmgr

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"insurance-benefit-agent-go/internal/cache"
	"insurance-benefit-agent-go/internal/config"
	"insurance-benefit-agent-go/internal/models"
	"insurance-benefit-agent-go/internal/payers"
)

func TestExpectedMFAToAddress(t *testing.T) {
	tests := []struct {
		name       string
		mailbox    string
		username   string
		expectedTo string
	}{
		{
			name:       "plus alias username",
			mailbox:    "mfains@hrdsq.com",
			username:   "mfains+976+dentaquest",
			expectedTo: "mfains+976+dentaquest@hrdsq.com",
		},
		{
			name:       "compact username",
			mailbox:    "mfains@hrdsq.com",
			username:   "mfains976dentaquest",
			expectedTo: "mfains+976+dentaquest@hrdsq.com",
		},
		{
			name:       "full email username",
			mailbox:    "mfains@hrdsq.com",
			username:   "custom@example.com",
			expectedTo: "custom@example.com",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := expectedMFAToAddress(tt.mailbox, tt.username)
			if got != tt.expectedTo {
				t.Fatalf("expectedMFAToAddress() = %q, want %q", got, tt.expectedTo)
			}
		})
	}
}

func TestNormalizeTriggerRequest(t *testing.T) {
	req := normalizeTriggerRequest(TriggerRequest{
		Action: " RUN_NOW ",
		PatNum: " 1235 ",
		PatientTargets: []PatientTarget{
			{PatNum: " 1235 ", AptNum: " 456 "},
		},
		RequestedBy: "  webhook ",
		Appointments: []models.Appointment{
			{PatNum: "1235", AptNum: "1", Ordinal: "1"},
			{PatNum: "1235", AptNum: "2", Ordinal: "1"},
			{PatNum: "1235", AptNum: "1", Ordinal: "2"},
		},
	})

	if req.Action != "run_now" {
		t.Fatalf("expected action run_now, got %q", req.Action)
	}
	if req.PatNum != "1235" {
		t.Fatalf("expected trimmed patnum, got %q", req.PatNum)
	}
	if len(req.PatNums) != 1 || req.PatNums[0] != "1235" {
		t.Fatalf("expected normalized patnums, got %v", req.PatNums)
	}
	if len(req.PatientTargets) != 1 || req.PatientTargets[0].AptNum != "456" {
		t.Fatalf("expected normalized targets, got %+v", req.PatientTargets)
	}
	if req.RequestedBy != "webhook" {
		t.Fatalf("expected trimmed requestedBy, got %q", req.RequestedBy)
	}
	if len(req.Appointments) != 3 {
		t.Fatalf("expected source appointments deduped by patient+appointment+insurance identity to 3 rows, got %d", len(req.Appointments))
	}
}

func TestValidateTriggerRequest(t *testing.T) {
	tests := []struct {
		name    string
		req     TriggerRequest
		wantErr bool
	}{
		{
			name: "valid run_all",
			req: TriggerRequest{
				Action:  "run_all",
				AddDays: 1,
			},
		},
		{
			name: "valid run_now",
			req: TriggerRequest{
				Action:         "run_now",
				PatientTargets: []PatientTarget{{PatNum: "1235"}, {PatNum: "1236", AptNum: "7"}},
			},
		},
		{
			name: "run_now missing patnum",
			req: TriggerRequest{
				Action: "run_now",
			},
			wantErr: true,
		},
		{
			name: "valid run_all today",
			req: TriggerRequest{
				Action:  "run_all",
				AddDays: 0,
			},
		},
		{
			name: "run_all invalid negative addDays",
			req: TriggerRequest{
				Action:  "run_all",
				AddDays: -1,
			},
			wantErr: true,
		},
		{
			name: "unsupported action",
			req: TriggerRequest{
				Action: "delete_everything",
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateTriggerRequest(tt.req)
			if tt.wantErr && err == nil {
				t.Fatalf("expected error, got nil")
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("expected nil error, got %v", err)
			}
		})
	}
}

func TestSortPendingRunsForProcessingPrioritizesRunNowBeforeRunAll(t *testing.T) {
	base := time.Date(2026, 5, 1, 8, 0, 0, 0, time.UTC)
	runs := []QueuedRun{
		{RunID: "run-all-old", Action: "run_all", ReceivedAt: base},
		{RunID: "run-now-new", Action: "run_now", PatNum: "123", ReceivedAt: base.Add(5 * time.Minute)},
		{RunID: "postprocess", Action: "run_all", Phase: PhaseProbed, ReceivedAt: base.Add(10 * time.Minute)},
		{RunID: "run-all-new", Action: "run_all", ReceivedAt: base.Add(15 * time.Minute)},
	}

	sortPendingRunsForProcessing(runs)

	got := []string{runs[0].RunID, runs[1].RunID, runs[2].RunID, runs[3].RunID}
	want := []string{"postprocess", "run-now-new", "run-all-old", "run-all-new"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("sorted run IDs = %v, want %v", got, want)
		}
	}
}

func TestCheckProbesDoneCountsProbeErrorAsDone(t *testing.T) {
	dir := t.TempDir()
	appts := []QueuedAppointment{
		{
			PayerURL: "DentalXchange.com",
			Appointment: models.Appointment{
				PatNum: "6699",
				AptNum: "121002",
			},
		},
		{
			PayerURL: "DentalXchange.com",
			Appointment: models.Appointment{
				PatNum: "6898",
				AptNum: "120783",
			},
		},
		{
			PayerURL: "DentalXchange.com",
			Appointment: models.Appointment{
				PatNum: "9999",
				AptNum: "120000",
			},
		},
	}

	files := []string{
		"DentalXchange.com_6699_121002_probe_error.json",
		"DentalXchange.com_6898_120783_api_probe.json",
	}
	for _, name := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("{}"), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}

	done, remaining := checkProbesDone(dir, appts)
	if len(done) != 2 {
		t.Fatalf("done=%d, want 2", len(done))
	}
	if len(remaining) != 1 {
		t.Fatalf("remaining=%d, want 1", len(remaining))
	}
	if remaining[0].Appointment.PatNum != "9999" {
		t.Fatalf("remaining patNum=%q, want 9999", remaining[0].Appointment.PatNum)
	}
}

func TestBucketAppointmentsByPayerKeepsSamePayerPrimaryAndSecondary(t *testing.T) {
	payers := []models.Payer{
		{PayerURL: "MetLife.com", PayerIDs: []string{"06126"}},
	}
	rows := []models.Appointment{
		{PatNum: "123", AptNum: "1", PayerID: "06126", Ordinal: "1"},
		{PatNum: "123", AptNum: "2", PayerID: "06126", Ordinal: "1"},
		{PatNum: "123", AptNum: "1", PayerID: "06126", Ordinal: "2"},
	}

	buckets := bucketAppointmentsByPayer(payers, rows)
	got := buckets.Supported["MetLife.com"]
	if len(got) != 3 {
		t.Fatalf("bucket kept %d appointments, want 3", len(got))
	}
	if got[0].Ordinal != "1" || got[1].Ordinal != "1" || got[2].Ordinal != "2" {
		t.Fatalf("bucket kept ordinals %q, %q, %q; want 1, 1, 2", got[0].Ordinal, got[1].Ordinal, got[2].Ordinal)
	}
}

func TestActivePayersAppliesLocalSkipBeforeBucketing(t *testing.T) {
	cfg, err := config.Default("")
	if err != nil {
		t.Fatalf("default config: %v", err)
	}
	cfg.Local.Payers.SkipPayerURLs = []string{"DeltaDentalIns.com", "UHCDental.com"}
	m := &Manager{cfg: cfg}

	active := m.activePayers([]models.Payer{
		{PayerURL: "DeltaDentalIns.com", PayerIDs: []string{"DD"}},
		{PayerURL: "UHCDental.com", PayerIDs: []string{"UHC"}},
		{PayerURL: "DentalXChange.com", PayerIDs: []string{"*"}},
	})

	if len(active) != 1 {
		t.Fatalf("active payers=%d, want 1", len(active))
	}
	if active[0].PayerURL != "DentalXChange.com" {
		t.Fatalf("active payer=%q, want DentalXChange.com", active[0].PayerURL)
	}

	buckets := bucketAppointmentsByPayer(active, []models.Appointment{
		{PatNum: "1", AptNum: "10", PayerID: "DD", Ordinal: "1"},
		{PatNum: "2", AptNum: "20", PayerID: "UHC", Ordinal: "1"},
	})
	if got := len(buckets.Supported["DentalXChange.com"]); got != 2 {
		t.Fatalf("DentalXChange bucket=%d, want 2", got)
	}
	if got := len(buckets.Supported["DeltaDentalIns.com"]); got != 0 {
		t.Fatalf("Delta bucket=%d, want 0", got)
	}
	if got := len(buckets.Supported["UHCDental.com"]); got != 0 {
		t.Fatalf("UHC bucket=%d, want 0", got)
	}
}

func TestRecordQueueResultsStoresOrdinalValues(t *testing.T) {
	run := QueuedRun{
		Appointments: []QueuedAppointment{
			{Appointment: models.Appointment{PatNum: "123", AptNum: "77777", Ordinal: "1"}},
			{Appointment: models.Appointment{PatNum: "123", AptNum: "77777", Ordinal: "2"}},
		},
	}

	run = recordQueueResults(run, []payers.PatientResult{
		{PatNum: "123", AptNum: "77777", Ordinal: "1", Status: "Verified"},
		{PatNum: "123", AptNum: "77777", Ordinal: "2", Status: "Inactive"},
	})

	if !run.Appointments[0].ResultComplete || run.Appointments[0].ResultValue != "V1" {
		t.Fatalf("primary result = complete %t value %q, want V1", run.Appointments[0].ResultComplete, run.Appointments[0].ResultValue)
	}
	if !run.Appointments[1].ResultComplete || run.Appointments[1].ResultValue != "NV2: Coverage found but inactive" {
		t.Fatalf("secondary result = complete %t value %q, want NV2 inactive", run.Appointments[1].ResultComplete, run.Appointments[1].ResultValue)
	}
	if !appointmentGroupResultsComplete(run.Appointments) {
		t.Fatal("expected appointment group results to be complete")
	}
}

func TestRecordQueueResultsSupportsNoAppointmentRows(t *testing.T) {
	run := QueuedRun{
		Appointments: []QueuedAppointment{
			{Appointment: models.Appointment{PatNum: "123", Ordinal: "1", InsSubNum: "700", PlanNum: "800"}},
		},
	}

	run = recordQueueResults(run, []payers.PatientResult{
		{PatNum: "123", Ordinal: "1", Status: "Verified"},
	})

	if !run.Appointments[0].ResultComplete || run.Appointments[0].ResultValue != "V1" {
		t.Fatalf("no-appointment result = complete %t value %q, want V1", run.Appointments[0].ResultComplete, run.Appointments[0].ResultValue)
	}
	if appointmentGroupKey(run.Appointments[0].Appointment) != "" {
		t.Fatal("no-appointment row should not have an apptfield group key")
	}
}

func TestRunNowNotificationAPIReadsConfig(t *testing.T) {
	got := runNowNotificationAPI(&cache.WorkSnapshot{
		OfficeKey: "office-123",
		ScraperConfig: &models.ScraperConfig{
			APIs: map[string]any{
				"runNowNotify": map[string]any{
					"url":   " https://example.test/notify ",
					"token": " token ",
				},
			},
		},
	})
	if got.URL != "https://example.test/api/config/schedule/get/insurance/notify/office-123" || got.Token != "token" {
		t.Fatalf("notification API=%+v", got)
	}
}

func TestRunNowNotificationAPIUsesPatconAuthFallback(t *testing.T) {
	got := runNowNotificationAPI(&cache.WorkSnapshot{
		OfficeKey: "office-123",
		ScraperConfig: &models.ScraperConfig{
			APIs: map[string]any{
				"runNowNotify": map[string]any{
					"url": "https://patcon.8px.us/api/config/schedule/get/insurance/notify/office-123",
				},
				"patcon": map[string]any{
					"url":   "https://patcon.8px.us",
					"token": "patcon-token",
				},
			},
		},
	})
	if got.URL != "https://patcon.8px.us/api/config/schedule/get/insurance/notify/office-123" || got.Token != "patcon-token" {
		t.Fatalf("notification API=%+v", got)
	}
}

func TestRunNowNotificationAPIDerivesPatconEndpoint(t *testing.T) {
	got := runNowNotificationAPI(&cache.WorkSnapshot{
		OfficeKey: "office-123",
		ScraperConfig: &models.ScraperConfig{
			APIs: map[string]any{
				"patcon": map[string]any{
					"url":   "https://patcon.8px.us/api/config/schedule/get/insurance",
					"token": "patcon-token",
				},
			},
		},
	})
	wantURL := "https://patcon.8px.us/api/config/schedule/get/insurance/notify/office-123"
	if got.URL != wantURL || got.Token != "patcon-token" {
		t.Fatalf("notification API=%+v, want url=%s token=patcon-token", got, wantURL)
	}
}

func TestBatchNotificationPayloadUsesZeroPatNum(t *testing.T) {
	got := batchNotificationPayload(3)
	if got.PatNum != "0" || got.PatName != "3 patients" {
		t.Fatalf("batch payload=%+v", got)
	}
}

func TestAuthorizationHeaderUsesBearerToken(t *testing.T) {
	for _, tc := range []struct {
		token string
		want  string
	}{
		{token: "raw-token", want: "Bearer raw-token"},
		{token: "Bearer already", want: "Bearer already"},
		{token: "Basic already", want: "Basic already"},
		{token: " ", want: ""},
	} {
		if got := authorizationHeader(tc.token); got != tc.want {
			t.Fatalf("authorizationHeader(%q)=%q want %q", tc.token, got, tc.want)
		}
	}
}

func TestPartitionProbeCompleteGroupsKeepsIncompleteOrdinalGroupPending(t *testing.T) {
	appts := []QueuedAppointment{
		{
			PayerURL:      "DeltaDentalIns.com",
			ProbeComplete: true,
			Appointment:   models.Appointment{PatNum: "123", AptNum: "77777", Ordinal: "1"},
		},
		{
			PayerURL:      "UHCDental.com",
			ProbeComplete: false,
			Appointment:   models.Appointment{PatNum: "123", AptNum: "77777", Ordinal: "2"},
		},
		{
			PayerURL:      "Dentical.com",
			ProbeComplete: true,
			Appointment:   models.Appointment{PatNum: "456", AptNum: "88888", Ordinal: "1"},
		},
	}

	ready, pending := partitionProbeCompleteGroups(appts)
	if len(ready) != 1 || ready[0].Appointment.PatNum != "456" {
		t.Fatalf("ready=%v, want only pat 456", ready)
	}
	if len(pending) != 2 {
		t.Fatalf("pending=%d, want primary+secondary group", len(pending))
	}
	if got := len(incompleteProbeAppointments(pending)); got != 1 {
		t.Fatalf("incomplete pending=%d, want only unresolved ordinal", got)
	}
}

func TestFallbackEligibleAppointmentsSkipsPatientErrors(t *testing.T) {
	dir := t.TempDir()
	appts := []models.Appointment{
		{PatNum: "100", AptNum: "1", Ordinal: "1"},
		{PatNum: "200", AptNum: "2", Ordinal: "1"},
		{PatNum: "300", AptNum: "3", Ordinal: "1"},
	}
	patientErr := payers.ProbeErrorArtifact{Error: "member not found", ErrorType: payers.ProbeErrorPatient}
	payerErr := payers.ProbeErrorArtifact{Error: "site unavailable", ErrorType: payers.ProbeErrorPayer}
	unsupportedErr := payers.ProbeErrorArtifact{Error: "payer lookup: no payer matched", ErrorType: payers.ProbeErrorUnsupported}
	writeProbeErrorArtifact(t, dir, "UHCDental.com", appts[0], patientErr)
	writeProbeErrorArtifact(t, dir, "UHCDental.com", appts[1], payerErr)
	writeProbeErrorArtifact(t, dir, "UHCDental.com", appts[2], unsupportedErr)

	got := fallbackEligibleAppointments(dir, "UHCDental.com", appts, nil)
	if len(got) != 1 {
		t.Fatalf("fallback eligible=%d, want 1", len(got))
	}
	if got[0].PatNum != "200" {
		t.Fatalf("fallback patNum=%q, want 200", got[0].PatNum)
	}
}

func TestCheckProbesDoneCountsUnsupportedProbeErrorAsDone(t *testing.T) {
	dir := t.TempDir()
	appts := []QueuedAppointment{{
		PayerURL: "DentalXChange.com",
		Appointment: models.Appointment{
			PatNum: "6875",
			AptNum: "120998",
		},
	}}
	path := filepath.Join(dir, "DentalXChange.com_6875_120998_probe_error.json")
	if err := os.WriteFile(path, []byte(`{"errorType":"unsupported_payer"}`), 0o644); err != nil {
		t.Fatalf("write probe error: %v", err)
	}

	done, remaining := checkProbesDone(dir, appts)
	if len(done) != 1 {
		t.Fatalf("done=%d, want 1", len(done))
	}
	if len(remaining) != 0 {
		t.Fatalf("remaining=%d, want 0", len(remaining))
	}
}

func TestReplaceQueuedAppointmentsWithFallbackSwapsOnlyFailedDirectRows(t *testing.T) {
	run := QueuedRun{
		Appointments: []QueuedAppointment{
			{PayerURL: "UHCDental.com", Appointment: models.Appointment{PatNum: "100", AptNum: "1", Ordinal: "1"}},
			{PayerURL: "UHCDental.com", Appointment: models.Appointment{PatNum: "200", AptNum: "2", Ordinal: "1"}},
			{PayerURL: "Dentical.com", Appointment: models.Appointment{PatNum: "300", AptNum: "3", Ordinal: "1"}},
		},
	}

	run = replaceQueuedAppointmentsWithFallback(run, "UHCDental.com", "DentalXChange.com", payers.ProbeErrorPayer, []models.Appointment{
		{PatNum: "200", AptNum: "2", Ordinal: "1"},
	})

	if len(run.Appointments) != 3 {
		t.Fatalf("appointments=%d, want 3", len(run.Appointments))
	}
	var foundFallback bool
	for _, queued := range run.Appointments {
		if queued.Appointment.PatNum != "200" {
			continue
		}
		foundFallback = true
		if queued.PayerURL != "DentalXChange.com" || queued.OriginalPayerURL != "UHCDental.com" || !queued.FallbackEligible {
			t.Fatalf("fallback queued row = %+v", queued)
		}
	}
	if !foundFallback {
		t.Fatal("expected fallback row for pat 200")
	}
}

func TestCheckProbesDoneRetriesPayerProbeError(t *testing.T) {
	dir := t.TempDir()
	appts := []QueuedAppointment{
		{
			PayerURL: "UHCDental.com",
			Appointment: models.Appointment{
				PatNum: "6812",
				AptNum: "120653",
			},
		},
	}
	path := filepath.Join(dir, "UHCDental.com_6812_120653_probe_error.json")
	if err := os.WriteFile(path, []byte(`{"errorType":"payer_error"}`), 0o644); err != nil {
		t.Fatalf("write probe error: %v", err)
	}

	done, remaining := checkProbesDone(dir, appts)
	if len(done) != 0 {
		t.Fatalf("done=%d, want 0", len(done))
	}
	if len(remaining) != 1 {
		t.Fatalf("remaining=%d, want 1", len(remaining))
	}
}

func writeProbeErrorArtifact(t *testing.T, dir, payerURL string, appt models.Appointment, artifact payers.ProbeErrorArtifact) {
	t.Helper()
	data, err := json.Marshal(artifact)
	if err != nil {
		t.Fatal(err)
	}
	path := payers.ProbeFilePath(dir, payerURL, appt.PatNum, appt.AptNum, "probe_error")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}
