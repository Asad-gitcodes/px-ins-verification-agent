package jobmgr

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"

	"insurance-benefit-agent-go/internal/cache"
	"insurance-benefit-agent-go/internal/models"
)

const runNowMissingInfoMessage = "Patient could not be verified. Missing info. Correct and verify."

type runNowPatientNotification struct {
	RunID       string                `json:"runId"`
	Action      string                `json:"action"`
	PatNum      string                `json:"patNum"`
	RequestedBy string                `json:"requestedBy"`
	Status      string                `json:"status"`
	Message     string                `json:"message,omitempty"`
	Results     []runNowPatientResult `json:"results,omitempty"`
	CompletedAt string                `json:"completedAt"`
}

type patconInsuranceNotification struct {
	PatNum  string `json:"patNum"`
	PatName string `json:"patName"`
}

type runNowPatientResult struct {
	Ordinal           string `json:"ordinal,omitempty"`
	PayerURL          string `json:"payerUrl,omitempty"`
	CarrierNum        string `json:"carrierNum,omitempty"`
	PayerID           string `json:"payerId,omitempty"`
	PlanNum           string `json:"planNum,omitempty"`
	InsSubNum         string `json:"insSubNum,omitempty"`
	AptNum            string `json:"aptNum,omitempty"`
	AppointmentStatus string `json:"appointmentStatus,omitempty"`
}

func (m *Manager) notifyRunNowNoRows(ctx context.Context, snapshot *cache.WorkSnapshot, runID string, req TriggerRequest, patNums []string, message string) {
	_ = runID
	_ = message
	patNums = normalizePatNums(patNums)
	if len(patNums) == 1 {
		m.notifyRunNowInsuranceRefresh(ctx, snapshot, patconInsuranceNotification{
			PatNum:  patNums[0],
			PatName: "Patient",
		})
		return
	}
	m.notifyRunNowInsuranceRefresh(ctx, snapshot, batchNotificationPayload(len(patNums)))
}

func (m *Manager) notifyCompletedRunNowPatients(ctx context.Context, snapshot *cache.WorkSnapshot, run QueuedRun) {
	if run.Action != "run_now" {
		return
	}
	requestedCount := runNowQueuedPatientCount(run)
	groups := make(map[string][]QueuedAppointment)
	order := make([]string, 0)
	for _, queued := range run.Appointments {
		patNum := strings.TrimSpace(queued.Appointment.PatNum)
		if patNum == "" {
			continue
		}
		if _, ok := groups[patNum]; !ok {
			order = append(order, patNum)
		}
		groups[patNum] = append(groups[patNum], queued)
	}
	if requestedCount > 1 {
		m.notifyRunNowInsuranceRefresh(ctx, snapshot, batchNotificationPayload(requestedCount))
		return
	}
	if len(order) == 1 {
		group := groups[order[0]]
		m.notifyRunNowInsuranceRefresh(ctx, snapshot, patconInsuranceNotification{
			PatNum:  order[0],
			PatName: patientNotificationName(group),
		})
		return
	}
	m.notifyRunNowInsuranceRefresh(ctx, snapshot, batchNotificationPayload(len(order)))
}

func (m *Manager) notifyRunNowInsuranceRefresh(ctx context.Context, snapshot *cache.WorkSnapshot, payload patconInsuranceNotification) {
	api := runNowNotificationAPI(snapshot)
	if api.URL == "" {
		log.Printf("[RunNowNotify] skipped patNum=%s patName=%q reason=notification API not configured", payload.PatNum, payload.PatName)
		return
	}
	if token := strings.TrimSpace(m.cfg.Bootstrap.Patcon.Token); token != "" {
		api.Token = token
	}
	body, err := json.Marshal(payload)
	if err != nil {
		log.Printf("[RunNowNotify] marshal failed patNum=%s: %v", payload.PatNum, err)
		return
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, api.URL, bytes.NewReader(body))
	if err != nil {
		log.Printf("[RunNowNotify] request build failed patNum=%s: %v", payload.PatNum, err)
		return
	}
	if auth := authorizationHeader(api.Token); auth != "" {
		req.Header.Set("Authorization", auth)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		log.Printf("[RunNowNotify] POST failed patNum=%s: %v", payload.PatNum, err)
		return
	}
	defer resp.Body.Close()
	text, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		log.Printf("[RunNowNotify] POST failed patNum=%s status=%s body=%s", payload.PatNum, resp.Status, strings.TrimSpace(string(text)))
		return
	}
	log.Printf("[RunNowNotify] sent patNum=%s patName=%q status=%s body=%q", payload.PatNum, payload.PatName, resp.Status, strings.TrimSpace(string(text)))
}

type notificationAPI struct {
	URL   string
	Token string
}

func runNowNotificationAPI(snapshot *cache.WorkSnapshot) notificationAPI {
	if snapshot == nil || snapshot.ScraperConfig == nil {
		return notificationAPI{}
	}
	raw, ok := snapshot.ScraperConfig.APIs["runNowNotify"]
	if !ok {
		raw, ok = snapshot.ScraperConfig.APIs["runNowNotification"]
	}
	if ok {
		apiMap, ok := raw.(map[string]any)
		if ok {
			url, _ := apiMap["url"].(string)
			token, _ := apiMap["token"].(string)
			if strings.TrimSpace(url) != "" {
				return notificationAPI{
					URL:   buildPatconInsuranceNotifyURL(url, snapshot.OfficeKey),
					Token: firstNonEmptyString(strings.TrimSpace(token), patconNotificationToken(snapshot)),
				}
			}
		}
	}
	patcon := patconNotificationBase(snapshot)
	if patcon == "" {
		return notificationAPI{}
	}
	return notificationAPI{
		URL:   buildPatconInsuranceNotifyURL(patcon, snapshot.OfficeKey),
		Token: patconNotificationToken(snapshot),
	}
}

func emptyAsOmitted(value string) string {
	return strings.TrimSpace(value)
}

func batchNotificationPayload(count int) patconInsuranceNotification {
	if count < 0 {
		count = 0
	}
	return patconInsuranceNotification{
		PatNum:  "0",
		PatName: fmt.Sprintf("%d patients", count),
	}
}

func runNowQueuedPatientCount(run QueuedRun) int {
	if len(run.PatNums) > 0 {
		return len(normalizePatNums(run.PatNums))
	}
	seen := map[string]struct{}{}
	for _, target := range run.PatientTargets {
		patNum := strings.TrimSpace(target.PatNum)
		if patNum != "" {
			seen[patNum] = struct{}{}
		}
	}
	if len(seen) > 0 {
		return len(seen)
	}
	for _, queued := range run.Appointments {
		patNum := strings.TrimSpace(queued.Appointment.PatNum)
		if patNum != "" {
			seen[patNum] = struct{}{}
		}
	}
	return len(seen)
}

func patientNotificationName(group []QueuedAppointment) string {
	for _, queued := range group {
		name := strings.TrimSpace(strings.TrimSpace(queued.Appointment.FName) + " " + strings.TrimSpace(queued.Appointment.LName))
		if name != "" {
			return name
		}
	}
	return "Patient"
}

func patconNotificationBase(snapshot *cache.WorkSnapshot) string {
	if snapshot == nil || snapshot.ScraperConfig == nil {
		return ""
	}
	if raw, ok := snapshot.ScraperConfig.APIs["patcon"]; ok {
		if apiMap, ok := raw.(map[string]any); ok {
			if rawURL, _ := apiMap["url"].(string); strings.TrimSpace(rawURL) != "" {
				return strings.TrimSpace(rawURL)
			}
		}
	}
	return strings.TrimSpace(snapshot.ConfigAPIURL)
}

func patconNotificationToken(snapshot *cache.WorkSnapshot) string {
	if snapshot == nil || snapshot.ScraperConfig == nil {
		return ""
	}
	if raw, ok := snapshot.ScraperConfig.APIs["patcon"]; ok {
		if apiMap, ok := raw.(map[string]any); ok {
			token, _ := apiMap["token"].(string)
			return strings.TrimSpace(token)
		}
	}
	return ""
}

func buildPatconInsuranceNotifyURL(rawBase, officeKey string) string {
	rawBase = strings.TrimSpace(rawBase)
	if rawBase == "" {
		return ""
	}
	if !strings.Contains(rawBase, "://") {
		rawBase = "https://" + rawBase
	}
	parsed, err := url.Parse(rawBase)
	if err != nil {
		return strings.TrimRight(rawBase, "/") + "/api/config/schedule/get/insurance/notify/" + url.PathEscape(officeKey)
	}
	if strings.HasSuffix(strings.TrimRight(parsed.Path, "/"), "/"+officeKey) {
		parsed.RawQuery = ""
		parsed.Fragment = ""
		return parsed.String()
	}
	parsed.Path = path.Join("/api/config/schedule/get/insurance/notify", officeKey)
	parsed.RawQuery = ""
	parsed.Fragment = ""
	return parsed.String()
}

func firstNonEmptyString(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func authorizationHeader(token string) string {
	token = strings.TrimSpace(token)
	if token == "" {
		return ""
	}
	lower := strings.ToLower(token)
	if strings.HasPrefix(lower, "bearer ") || strings.HasPrefix(lower, "basic ") {
		return token
	}
	return "Bearer " + token
}

func missingPatNums(requested []string, rows []models.Appointment) []string {
	found := map[string]struct{}{}
	for _, row := range rows {
		if strings.TrimSpace(row.PatNum) != "" {
			found[strings.TrimSpace(row.PatNum)] = struct{}{}
		}
	}
	var missing []string
	for _, patNum := range requested {
		if _, ok := found[strings.TrimSpace(patNum)]; !ok {
			missing = append(missing, patNum)
		}
	}
	return missing
}

func unsupportedPatNums(rows []models.Appointment) []string {
	seen := map[string]struct{}{}
	var out []string
	for _, row := range rows {
		patNum := strings.TrimSpace(row.PatNum)
		if patNum == "" {
			continue
		}
		if _, ok := seen[patNum]; ok {
			continue
		}
		seen[patNum] = struct{}{}
		out = append(out, patNum)
	}
	return out
}

func unsupportedPatNumsFromBuckets(rows map[string][]models.Appointment) []string {
	var flat []models.Appointment
	for _, appointments := range rows {
		flat = append(flat, appointments...)
	}
	return unsupportedPatNums(flat)
}

func notificationErrorMessage(err error) string {
	if err == nil {
		return ""
	}
	return fmt.Sprint(err)
}
