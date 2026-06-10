// Package resultwriter handles the two write-back actions that follow a
// successful eligibility scrape: updating the Open Dental appointment field
// and uploading the eligibility PDF.
//
// Both actions are gated by TestingConfig flags so they can be disabled
// during local testing without changing any other code paths.
package resultwriter

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"os"
	"strings"
	"time"

	"insurance-benefit-agent-go/internal/config"
	"insurance-benefit-agent-go/internal/models"
)

// ApptStatus values written to the Open Dental HRDView apptfield.
// These are client-facing strings — keep exact casing.
const (
	ApptStatusVerified           = "Verified"
	ApptStatusInactive           = "Inactive"
	ApptStatusNotFound           = "Not Found"
	ApptStatusError              = "Error"
	ApptStatusPayerSystemFailure = "Payer/System Failure"
)

const (
	apptFieldVerified           = "V1"
	apptFieldInactive           = "NV1: Coverage found but inactive"
	apptFieldNotFound           = "NV1: Patient/member not found"
	apptFieldInvalidMemberInfo  = "NV1: Invalid/missing member info"
	apptFieldPayerSystemFailure = "NV1: Payer/system failure"
)

// PatconConfig holds the URL and auth token for the patcon write-back API,
// extracted from ScraperConfig.APIs["patcon"].
type PatconConfig struct {
	URL   string
	Token string
}

// Writer performs the post-scrape write-back actions.
type Writer struct {
	testing config.TestingConfig
	patcon  PatconConfig
	client  *http.Client
}

// New creates a Writer. patcon is extracted from ScraperConfig.APIs["patcon"].
func New(testing config.TestingConfig, scraperAPIs map[string]any) (*Writer, error) {
	patcon, err := extractPatconConfig(scraperAPIs)
	if err != nil {
		return nil, err
	}
	return &Writer{
		testing: testing,
		patcon:  patcon,
		client:  &http.Client{Timeout: 30 * time.Second},
	}, nil
}

// EligibilityStatus maps a successful scrape outcome to an apptfield status.
// Use ApptStatusNotFound or ApptStatusError directly for those outcomes.
func EligibilityStatus(isEligible bool) string {
	if isEligible {
		return ApptStatusVerified
	}
	return ApptStatusInactive
}

// AppointmentFieldValue maps an internal outcome to the client-facing
// HRDView apptfield value. For primary insurance, only active coverage
// writes V1; every non-active outcome writes NV1 with an office-readable reason.
func AppointmentFieldValue(status string) string {
	return AppointmentOrdinalFieldValue(status, "1")
}

func AppointmentOrdinalFieldValue(status string, ordinal string) string {
	ordinal = strings.TrimSpace(ordinal)
	if ordinal == "" {
		ordinal = "1"
	}
	prefix := "NV" + ordinal + ": "
	switch status {
	case ApptStatusVerified:
		return "V" + ordinal
	case ApptStatusInactive:
		return prefix + "Coverage found but inactive"
	case ApptStatusNotFound:
		return prefix + "Patient/member not found"
	case ApptStatusPayerSystemFailure:
		return prefix + "Payer/system failure"
	default:
		return prefix + "Invalid/missing member info"
	}
}

func StatusForProbeErrorType(errorType string) string {
	switch strings.TrimSpace(errorType) {
	case "payer_error", "system_error":
		return ApptStatusPayerSystemFailure
	default:
		return ApptStatusError
	}
}

func (w *Writer) ApplyAppointmentFieldValue(appointment models.Appointment, officeKey string, fieldValue string) {
	if strings.TrimSpace(appointment.AptNum) == "" {
		log.Printf("[ResultWriter] aptField skipped patNum=%s reason=no appointment", appointment.PatNum)
		return
	}
	if w.testing.ShouldUpdateApptField() {
		log.Printf("[ResultWriter] aptField aptNum=%s value=%q", appointment.AptNum, fieldValue)
		if err := w.saveAppointmentField(officeKey, appointment.AptNum, fieldValue); err != nil {
			log.Printf("[ResultWriter] apptField FAILED aptNum=%s: %v", appointment.AptNum, err)
		}
	} else {
		log.Printf("[ResultWriter] aptField aptNum=%s value=%q (skipped)", appointment.AptNum, fieldValue)
	}
}

// ApplyResult updates the appointment field and uploads the PDF (when enabled).
// status must be one of the ApptStatus constants — the adapter is responsible for mapping its outcome.
// writePDF is pre-resolved by the caller from insPDFGenerate and local config.
// pdfBytes may be nil (PDF skipped for non-verified outcomes or when generation is disabled).
func (w *Writer) ApplyResult(appointment models.Appointment, status string, officeKey string, pdfBytes []byte, writePDF bool) {
	fieldValue := AppointmentFieldValue(status)
	if strings.TrimSpace(appointment.AptNum) == "" {
		log.Printf("[ResultWriter] aptField skipped patNum=%s value=%q reason=no appointment", appointment.PatNum, fieldValue)
	} else {
		if w.testing.ShouldUpdateApptField() {
			log.Printf("[ResultWriter] aptField aptNum=%s value=%q", appointment.AptNum, fieldValue)
			if err := w.saveAppointmentField(officeKey, appointment.AptNum, fieldValue); err != nil {
				log.Printf("[ResultWriter] apptField FAILED aptNum=%s: %v", appointment.AptNum, err)
			}
		} else {
			log.Printf("[ResultWriter] aptField aptNum=%s value=%q (skipped)", appointment.AptNum, fieldValue)
		}
	}

	if writePDF && len(pdfBytes) > 0 {
		w.ApplyPDF(appointment, status, officeKey, pdfBytes, writePDF)
	}
}

// ApplyPDF writes/uploads only the PDF portion of a result. It is used by the
// deferred PDF phase so payer sessions can finish before Chrome is launched for
// rendering.
func (w *Writer) ApplyPDF(appointment models.Appointment, status string, officeKey string, pdfBytes []byte, writePDF bool) {
	if !writePDF || len(pdfBytes) == 0 {
		return
	}
	if w.testing.ShouldUsePDFLocalOnly() {
		if err := w.writePDFLocal(appointment.PatNum, appointment.AptNum, status, pdfBytes); err != nil {
			log.Printf("[ResultWriter] PDF local write FAILED patNum=%s: %v", appointment.PatNum, err)
		}
	} else {
		if err := w.uploadPDF(officeKey, appointment, status, pdfBytes); err != nil {
			log.Printf("[ResultWriter] PDF upload FAILED patNum=%s: %v", appointment.PatNum, err)
		}
	}
}

// ── local PDF write ───────────────────────────────────────────────────────────

func (w *Writer) writePDFLocal(patNum, aptNum, status string, pdfBytes []byte) error {
	if err := os.MkdirAll("pdfs", 0755); err != nil {
		return fmt.Errorf("create pdfs dir: %w", err)
	}
	fileName := fmt.Sprintf("pdfs/pat%s_apt%s_%s.pdf", patNum, aptNum, status)
	if err := os.WriteFile(fileName, pdfBytes, 0644); err != nil {
		return fmt.Errorf("write pdf: %w", err)
	}
	log.Printf("[ResultWriter] PDF saved locally: %s", fileName)
	return nil
}

// ── apptField ────────────────────────────────────────────────────────────────

func (w *Writer) saveAppointmentField(officeKey, aptNum, fieldValue string) error {
	url := fmt.Sprintf("%s/api/config/save/apptField/%s", w.patcon.URL, officeKey)
	body := fmt.Sprintf(
		`{"aptnum":%q,"fieldName":"HRDView","fieldType":"0","pickList":"","fieldFor":%q}`,
		aptNum, fieldValue,
	)

	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", w.patcon.Token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := w.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	text, _ := io.ReadAll(resp.Body)
	log.Printf("[ResultWriter] apptField response status=%s body=%s", resp.Status, strings.TrimSpace(string(text)))

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(text)))
	}
	return nil
}

// ── PDF upload ────────────────────────────────────────────────────────────────

func (w *Writer) uploadPDF(officeKey string, appointment models.Appointment, status string, pdfBytes []byte) error {
	defNum, err := w.resolveUploadDefNum(officeKey)
	if err != nil {
		return fmt.Errorf("resolve upload defNum: %w", err)
	}
	log.Printf("[ResultWriter] resolved upload defNum=%s officeKey=%s", defNum, officeKey)

	patNum := appointment.PatNum
	fileName := buildPDFFileName(status, appointment.Ordinal)
	url := fmt.Sprintf("%s/api/config/upload/file/%s", w.patcon.URL, officeKey)
	log.Printf("[ResultWriter] PDF upload POST %s fileName=%s patNum=%s defNum=%s size=%d bytes", url, fileName, patNum, defNum, len(pdfBytes))

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)

	_ = writeField(mw, "originalFileName", fileName)
	_ = writeField(mw, "patnum", patNum)
	_ = writeField(mw, "doccat", defNum)

	fh := make(textproto.MIMEHeader)
	fh.Set("Content-Disposition", fmt.Sprintf(`form-data; name="file"; filename=%q`, fileName))
	fh.Set("Content-Type", "application/pdf")
	fw, err := mw.CreatePart(fh)
	if err != nil {
		return err
	}
	if _, err := fw.Write(pdfBytes); err != nil {
		return err
	}
	mw.Close()

	req, err := http.NewRequest(http.MethodPost, url, &buf)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", w.patcon.Token)
	req.Header.Set("Content-Type", mw.FormDataContentType())

	resp, err := w.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	text, _ := io.ReadAll(resp.Body)
	log.Printf("[ResultWriter] PDF upload response status=%s body=%s", resp.Status, strings.TrimSpace(string(text)))

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(text)))
	}
	return nil
}

func (w *Writer) resolveUploadDefNum(officeKey string) (string, error) {
	url := fmt.Sprintf("%s/api/config/doctor/foldersname/%s", w.patcon.URL, officeKey)
	log.Printf("[ResultWriter] resolving upload defNum GET %s", url)
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", w.patcon.Token)

	resp, err := w.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		text, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(text)))
	}

	var items []map[string]any
	if err := decodeJSON(resp.Body, &items); err != nil {
		return "", fmt.Errorf("parse upload config: %w", err)
	}

	priority := []string{"Insurance", "px-misc", "Miscellaneous"}
	byName := make(map[string]map[string]any, len(items))
	for _, item := range items {
		if name, ok := item["ItemName"].(string); ok {
			byName[name] = item
		}
	}
	for _, name := range priority {
		if item, ok := byName[name]; ok {
			switch v := item["DefNum"].(type) {
			case float64:
				return fmt.Sprintf("%.0f", v), nil
			case string:
				return v, nil
			}
		}
	}
	return "", fmt.Errorf("no upload DefNum found for office=%s", officeKey)
}

// ── helpers ───────────────────────────────────────────────────────────────────

func buildPDFFileName(status string, ordinal string) string {
	prefix := "PXV2_Primary_Electronic_Benefits"
	if strings.TrimSpace(ordinal) == "2" {
		prefix = "PXV2_Secondary_Electronic_Benefits"
	}
	suffix := "NotActive"
	switch status {
	case ApptStatusVerified:
		suffix = "Active"
	}
	return fmt.Sprintf("%s_%s.pdf", prefix, suffix)
}

func writeField(mw *multipart.Writer, field, value string) error {
	fw, err := mw.CreateFormField(field)
	if err != nil {
		return err
	}
	_, err = io.WriteString(fw, value)
	return err
}

func extractPatconConfig(apis map[string]any) (PatconConfig, error) {
	raw, ok := apis["patcon"]
	if !ok {
		return PatconConfig{}, fmt.Errorf("patcon API config not found in ScraperConfig.APIs")
	}
	m, ok := raw.(map[string]any)
	if !ok {
		return PatconConfig{}, fmt.Errorf("patcon API config has unexpected type")
	}
	url, _ := m["url"].(string)
	token, _ := m["token"].(string)
	if url == "" || token == "" {
		return PatconConfig{}, fmt.Errorf("patcon API config missing url or token")
	}
	return PatconConfig{URL: url, Token: token}, nil
}

func decodeJSON(r io.Reader, v any) error {
	return json.NewDecoder(r).Decode(v)
}
