package jobmgr

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"insurance-benefit-agent-go/internal/models"
	"insurance-benefit-agent-go/internal/odetrans"
	"insurance-benefit-agent-go/internal/payers"
)

const defaultQueueDir = "queue"
const maxQueueRetries = 2
const normalRetryInterval = 5 * time.Minute
const payerDownRetryInterval = 10 * time.Minute

// Queue run phases — persisted to disk so crash recovery knows where to resume.
const (
	PhaseProbing        = "probing"        // Phase 1: browser scraping in progress
	PhaseProbed         = "probed"         // Phase 1 done, probe files written
	PhasePostprocessing = "postprocessing" // Phase 2: building eligibility + results
)

// QueuedAppointment pairs a full appointment with the resolved PayerURL,
// so retries can replay exact appointments regardless of the current date.
type QueuedAppointment struct {
	PayerURL         string             `json:"payerUrl"`
	Appointment      models.Appointment `json:"appointment"`
	ProbeComplete    bool               `json:"probeComplete,omitempty"`
	ResultComplete   bool               `json:"resultComplete,omitempty"`
	ResultValue      string             `json:"resultValue,omitempty"`
	FallbackEligible bool               `json:"fallbackEligible,omitempty"`
	OriginalPayerURL string             `json:"originalPayerUrl,omitempty"`
	FallbackReason   string             `json:"fallbackReason,omitempty"`
}

// QueuedRun is the durable record written to disk before any browser is opened.
// It survives agent restarts and drives the phase lifecycle and retry logic.
type QueuedRun struct {
	RunID              string                  `json:"runId"`
	Action             string                  `json:"action"`
	PatNum             string                  `json:"patNum,omitempty"`
	PatNums            []string                `json:"patNums,omitempty"`
	PatientTargets     []PatientTarget         `json:"data,omitempty"`
	AddDays            int                     `json:"addDays,omitempty"`
	RequestedBy        string                  `json:"requestedBy"`
	ReceivedAt         time.Time               `json:"receivedAt"`
	RetryCount         int                     `json:"retryCount"`
	NextRetryAt        *time.Time              `json:"nextRetryAt,omitempty"`
	Appointments       []QueuedAppointment     `json:"appointments"`
	SourceAppointments []models.Appointment    `json:"sourceAppointments,omitempty"`
	OfficeIdentity     odetrans.OfficeIdentity `json:"officeIdentity,omitempty"`
	Phase              string                  `json:"phase,omitempty"` // probing → probed → postprocessing → (deleted)
}

func persistQueueFile(dir string, run QueuedRun) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create queue dir: %w", err)
	}
	data, err := json.MarshalIndent(run, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal queue run: %w", err)
	}
	return os.WriteFile(filepath.Join(dir, run.RunID+".json"), data, 0o644)
}

func removeQueueFile(dir, runID string) error {
	err := os.Remove(filepath.Join(dir, runID+".json"))
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

func loadQueueFile(dir, runID string) (*QueuedRun, error) {
	data, err := os.ReadFile(filepath.Join(dir, runID+".json"))
	if err != nil {
		return nil, err
	}
	var run QueuedRun
	if err := json.Unmarshal(data, &run); err != nil {
		return nil, err
	}
	return &run, nil
}

func loadPendingQueueFiles(dir string) ([]QueuedRun, error) {
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read queue dir: %w", err)
	}
	var runs []QueuedRun
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		data, readErr := os.ReadFile(filepath.Join(dir, entry.Name()))
		if readErr != nil {
			continue
		}
		var run QueuedRun
		if jsonErr := json.Unmarshal(data, &run); jsonErr != nil {
			continue
		}
		runs = append(runs, run)
	}
	sort.Slice(runs, func(i, j int) bool {
		return runs[i].ReceivedAt.Before(runs[j].ReceivedAt)
	})
	return runs, nil
}

// checkProbesDone partitions appointments into done (probe file found) and remaining (not found).
// Error probe files count as done because they are patient-level outcomes that
// phase 2 converts into Error/Unable to Determine instead of retrying forever.
func checkProbesDone(probeDir string, appts []QueuedAppointment) (done, remaining []QueuedAppointment) {
	for _, a := range appts {
		pattern := filepath.Join(probeDir, fmt.Sprintf("%s_%s_%s_*.json",
			payers.SanitizeProbeSegment(a.PayerURL),
			payers.SanitizeProbeSegment(a.Appointment.PatNum),
			payers.SanitizeProbeSegment(payers.ProbeAppointmentSegment(a.Appointment)),
		))
		matches, _ := filepath.Glob(pattern)
		hasProbe := false
		for _, m := range matches {
			name := filepath.Base(m)
			if strings.HasSuffix(name, "_probe_error.json") {
				if probeErrorCountsAsDone(m) {
					hasProbe = true
					break
				}
				continue
			}
			if strings.HasSuffix(name, "_probe.json") ||
				strings.HasSuffix(name, "_api_probe.json") {
				hasProbe = true
				break
			}
		}
		if hasProbe {
			done = append(done, a)
		} else {
			remaining = append(remaining, a)
		}
	}
	return
}

func probeErrorCountsAsDone(path string) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return true
	}
	var payload struct {
		ErrorType string `json:"errorType"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return true
	}
	switch strings.TrimSpace(payload.ErrorType) {
	case "", "patient_error", "unknown_error":
		return true
	case "payer_error", "system_error":
		return false
	default:
		return true
	}
}

// detectPayerDown returns true when all remaining appointments belong to the same payer,
// indicating the payer site is likely down rather than individual patient issues.
func detectPayerDown(remaining []QueuedAppointment) bool {
	if len(remaining) == 0 {
		return false
	}
	first := strings.ToLower(remaining[0].PayerURL)
	for _, a := range remaining[1:] {
		if strings.ToLower(a.PayerURL) != first {
			return false
		}
	}
	return true
}
