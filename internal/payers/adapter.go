package payers

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"

	"insurance-benefit-agent-go/internal/advanced"
	"insurance-benefit-agent-go/internal/config"
	"insurance-benefit-agent-go/internal/mfa"
	"insurance-benefit-agent-go/internal/models"
	"insurance-benefit-agent-go/internal/odetrans"
)

type DeferredPDFTask struct {
	PayerURL    string
	Appointment models.Appointment
	Status      string
	OfficeKey   string
	Report      *advanced.PatientEligibilityReport
	OutputDir   string
	WritePDF    bool
}

type SessionInput struct {
	Payer              models.Payer
	Credential         models.CredentialCandidate
	Password           string
	EmailMFA           *mfa.EmailConfig
	Appointments       []models.Appointment
	ScraperConfig      *models.ScraperConfig
	AppointmentDays    int
	RequestedOfficeKey string
	Testing            config.TestingConfig
	Headless           bool     // true in production; false for local debugging via agent.local.json
	OfficeCodes        []string // CDT codes from the office standing code list (PatCon API)
	WritePDF           bool     // insPDFGenerate (server) AND testing.writePdf (local) must both allow it
	AllowEDIWriteBack  bool     // controls OD eTrans DB insert independently from adapter apptfield writes
	ProbeOutputDir     string   // shared phase-1 raw probe folder for the current run
	OfficeIdentity     odetrans.OfficeIdentity
	SkipProbing        bool // when true, skip Phase 1 and run Phase 2 from existing probe files
	EnqueuePDF         func(DeferredPDFTask)
	// PatchCredentialFn persists a discovered providerName back to the snapshot
	// so subsequent runs skip the UI-based discovery step. May be nil.
	PatchCredentialFn func(payerURL, providerName string)
}

func ProbeRunDir(runStamp string) string {
	_ = runStamp
	return filepath.Join("artifacts", "_probe_bucket")
}

func ProbeFilePath(dir, payerURL, patNum, aptNum, suffix string) string {
	return filepath.Join(dir, fmt.Sprintf("%s_%s_%s_%s.json",
		sanitizeProbeSegment(payerURL),
		sanitizeProbeSegment(patNum),
		sanitizeProbeSegment(aptNum),
		sanitizeProbeSegment(suffix),
	))
}

func ProbeFilePathForAppointment(dir, payerURL string, appointment models.Appointment, suffix string) string {
	return ProbeFilePath(dir, payerURL, appointment.PatNum, ProbeAppointmentSegment(appointment), suffix)
}

func ProbeAppointmentSegment(appointment models.Appointment) string {
	if strings.TrimSpace(appointment.AptNum) != "" {
		return appointment.AptNum
	}
	parts := []string{"noappt", "ord" + strings.TrimSpace(firstNonEmptyString(appointment.Ordinal, "1"))}
	if strings.TrimSpace(appointment.InsSubNum) != "" {
		parts = append(parts, "sub"+strings.TrimSpace(appointment.InsSubNum))
	}
	if strings.TrimSpace(appointment.PlanNum) != "" {
		parts = append(parts, "plan"+strings.TrimSpace(appointment.PlanNum))
	}
	return strings.Join(parts, "_")
}

func firstNonEmptyString(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func (input SessionInput) QueuePDFTask(appointment models.Appointment, status string, report *advanced.PatientEligibilityReport, outputDir string) {
	if report != nil {
		report = ApplyStatusProvenance(report, appointment, input.Payer.PayerURL)
		pair, written, err := odetrans.WritePairFiles(outputDir, odetrans.BuildInput{
			Appointment: appointment,
			Report:      report,
			Status:      status,
			PayerURL:    input.Payer.PayerURL,
			Credential:  input.Credential,
			Provider:    input.OfficeIdentity.Provider,
			Practice:    input.OfficeIdentity.Practice,
		})
		if err != nil {
			log.Printf("[ODEtrans] EDI artifact write FAILED payerUrl=%s patNum=%s aptNum=%s ordinal=%s: %v",
				input.Payer.PayerURL, appointment.PatNum, appointment.AptNum, appointment.Ordinal, err)
		} else if written {
			log.Printf("[ODEtrans] EDI artifacts written payerUrl=%s patNum=%s aptNum=%s ordinal=%s",
				input.Payer.PayerURL, appointment.PatNum, appointment.AptNum, appointment.Ordinal)
			if input.AllowEDIWriteBack {
				ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
				result, err := odetrans.PersistPair(ctx, input.ScraperConfig, input.RequestedOfficeKey, appointment, pair, time.Now())
				cancel()
				if err != nil {
					log.Printf("[ODEtrans] OD insert FAILED payerUrl=%s patNum=%s aptNum=%s ordinal=%s: %v",
						input.Payer.PayerURL, appointment.PatNum, appointment.AptNum, appointment.Ordinal, err)
				} else {
					log.Printf("[ODEtrans] OD insert complete payerUrl=%s patNum=%s aptNum=%s ordinal=%s etrans270=%d etrans271=%d",
						input.Payer.PayerURL, appointment.PatNum, appointment.AptNum, appointment.Ordinal, result.Etrans270, result.Etrans271)
				}
			} else {
				log.Printf("[ODEtrans] OD insert skipped payerUrl=%s patNum=%s aptNum=%s reason=write-back disabled",
					input.Payer.PayerURL, appointment.PatNum, appointment.AptNum)
			}
		}
	}
	if input.EnqueuePDF == nil {
		log.Printf("[PDFQueue] skipped payerUrl=%s patNum=%s aptNum=%s reason=no enqueue function", input.Payer.PayerURL, appointment.PatNum, appointment.AptNum)
		return
	}
	if !input.WritePDF {
		log.Printf("[PDFQueue] skipped payerUrl=%s patNum=%s aptNum=%s reason=writePdf false", input.Payer.PayerURL, appointment.PatNum, appointment.AptNum)
		return
	}
	if report == nil {
		log.Printf("[PDFQueue] skipped payerUrl=%s patNum=%s aptNum=%s reason=nil report", input.Payer.PayerURL, appointment.PatNum, appointment.AptNum)
		return
	}
	input.EnqueuePDF(DeferredPDFTask{
		PayerURL:    input.Payer.PayerURL,
		Appointment: appointment,
		Status:      status,
		OfficeKey:   input.RequestedOfficeKey,
		Report:      report,
		OutputDir:   outputDir,
		WritePDF:    input.WritePDF,
	})
}

// SanitizeProbeSegment is the exported form of sanitizeProbeSegment for use outside this package.
func SanitizeProbeSegment(value string) string { return sanitizeProbeSegment(value) }

func sanitizeProbeSegment(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "unknown"
	}
	var builder strings.Builder
	for _, r := range value {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			builder.WriteRune(r)
		case r == '-' || r == '_' || r == '.':
			builder.WriteRune(r)
		default:
			builder.WriteByte('_')
		}
	}
	return strings.Trim(builder.String(), "._-")
}

// PatientResult holds the per-patient outcome recorded during a payer run.
type PatientResult struct {
	PatNum  string `json:"patNum"`
	AptNum  string `json:"aptNum"`
	Ordinal string `json:"ordinal,omitempty"`
	Status  string `json:"status"`
}

// RunSummary holds per-outcome counts for one payer run.
// Patient-level outcomes (Verified, Inactive, NotFound, PatientError) are
// normal results and are never treated as run failures.
type RunSummary struct {
	Verified     int
	Inactive     int
	NotFound     int
	PatientError int // payer site returned no usable data for this patient
	Results      []PatientResult
}

func (s RunSummary) Total() int {
	return s.Verified + s.Inactive + s.NotFound + s.PatientError
}

func (s RunSummary) Processed() int {
	return s.Verified + s.Inactive + s.NotFound + s.PatientError
}

// Record increments the correct counter for a resultwriter status string.
func (s *RunSummary) Record(status string) {
	switch status {
	case "Verified":
		s.Verified++
	case "Inactive":
		s.Inactive++
	case "Not Found":
		s.NotFound++
	default:
		s.PatientError++
	}
}

// RecordPatient increments the counter and appends a per-patient result entry.
func (s *RunSummary) RecordPatient(patNum, aptNum, status string) {
	s.Record(status)
	s.Results = append(s.Results, PatientResult{PatNum: patNum, AptNum: aptNum, Status: status})
}

// RecordAppointment increments the counter and appends a result with ordinal
// context so appointment-field write-back can coordinate primary/secondary.
func (s *RunSummary) RecordAppointment(appointment models.Appointment, status string) {
	s.Record(status)
	s.Results = append(s.Results, PatientResult{
		PatNum:  appointment.PatNum,
		AptNum:  appointment.AptNum,
		Ordinal: strings.TrimSpace(appointment.Ordinal),
		Status:  status,
	})
}

type Adapter interface {
	// PayerURL is the canonical server contract value, such as DentaQuest.com.
	PayerURL() string
	// Supports lets one adapter accept aliases later without changing jobmgr.
	Supports(payerURL string) bool
	// Run is the payer-specific session pipeline boundary. Shared bootstrap,
	// config, caching, and status reporting stay outside adapters.
	Run(ctx context.Context, input SessionInput) (RunSummary, error)
}

// ScanProbeAppointments lists probe files written by a payer in probeDir and
// returns minimal Appointment stubs (PatNum + AptNum only) for each one found.
// The probe filename format is: {payerURL}_{patNum}_{aptNum}_{suffix}.json
func ScanProbeAppointments(probeDir, payerURL, suffix string) []models.Appointment {
	prefix := sanitizeProbeSegment(payerURL) + "_"
	tail := "_" + sanitizeProbeSegment(suffix) + ".json"

	entries, err := os.ReadDir(probeDir)
	if err != nil {
		return nil
	}

	var result []models.Appointment
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !strings.HasPrefix(name, prefix) || !strings.HasSuffix(name, tail) {
			continue
		}
		middle := name[len(prefix) : len(name)-len(tail)]
		idx := strings.Index(middle, "_")
		if idx < 0 {
			continue
		}
		patNum := middle[:idx]
		aptNum := middle[idx+1:]
		if patNum == "" || aptNum == "" {
			continue
		}
		result = append(result, models.Appointment{PatNum: patNum, AptNum: aptNum})
	}
	return result
}
