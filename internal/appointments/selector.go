package appointments

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"insurance-benefit-agent-go/internal/models"
)

const defaultQueryLimit = 500

type Selector struct {
	httpClient *http.Client
	queryLimit int
}

type QueryAPIConfig struct {
	URL   string
	Token string
}

type SelectRequest struct {
	OfficeKey                     string
	PayerURL                      string
	PayerIDs                      []string
	FutureRangeDays               int
	RetryErrorsOnly               bool
	IgnoreAppointmentStatusFilter bool
	ScraperConfig                 *models.ScraperConfig
}

type PatientSelectRequest struct {
	OfficeKey     string
	PatNum        string
	PatNums       []string
	Targets       []PatientTarget
	ScraperConfig *models.ScraperConfig
}

type PatientTarget struct {
	PatNum string
	AptNum string
}

type DaySelectRequest struct {
	OfficeKey                     string
	PayerIDs                      []string
	AddDays                       int
	RetryErrorsOnly               bool
	IgnoreAppointmentStatusFilter bool
	ScraperConfig                 *models.ScraperConfig
}

type queryResponse struct {
	ResultData []models.Appointment `json:"ResultData"`
	Data       []models.Appointment `json:"data"`
	Rows       []models.Appointment `json:"rows"`
	Results    []models.Appointment `json:"results"`
}

func NewSelector(queryLimit int) *Selector {
	if queryLimit <= 0 {
		queryLimit = defaultQueryLimit
	}
	return &Selector{
		httpClient: &http.Client{Timeout: 60 * time.Second},
		queryLimit: queryLimit,
	}
}

func (s *Selector) SelectForPayer(ctx context.Context, req SelectRequest) ([]models.Appointment, error) {
	apiConfig, err := queryAPIConfig(req.ScraperConfig)
	if err != nil {
		return nil, err
	}
	if len(req.PayerIDs) == 0 {
		return nil, fmt.Errorf("no payer IDs configured for payerUrl=%s", req.PayerURL)
	}

	query := s.buildAppointmentQuery(req)
	body := map[string]any{
		"key":   req.OfficeKey,
		"query": query,
	}
	encodedBody, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("marshal appointment query: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, apiConfig.URL, bytes.NewReader(encodedBody))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Authorization", apiConfig.Token)
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json")

	resp, err := s.httpClient.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		if len(body) > 0 {
			return nil, fmt.Errorf("appointment query failed with status %s: %s", resp.Status, bytes.TrimSpace(body))
		}
		return nil, fmt.Errorf("appointment query failed with status %s", resp.Status)
	}

	rows, err := decodeAppointments(resp.Body)
	if err != nil {
		return nil, err
	}

	return dedupeByPatNumOrdinal(rows), nil
}

func (s *Selector) SelectForPatient(ctx context.Context, req PatientSelectRequest) ([]models.Appointment, error) {
	if strings.TrimSpace(req.PatNum) != "" && len(req.PatNums) == 0 {
		req.PatNums = []string{req.PatNum}
	}
	return s.SelectForPatients(ctx, req)
}

func (s *Selector) SelectForPatients(ctx context.Context, req PatientSelectRequest) ([]models.Appointment, error) {
	apiConfig, err := queryAPIConfig(req.ScraperConfig)
	if err != nil {
		return nil, err
	}
	targets := normalizePatientTargets(req.Targets)
	if len(targets) == 0 {
		for _, patNum := range normalizePatNums(req.PatNums) {
			targets = append(targets, PatientTarget{PatNum: patNum})
		}
	}
	if len(targets) == 0 {
		return nil, fmt.Errorf("patnum is required")
	}
	rows, err := s.runQuery(ctx, apiConfig, req.OfficeKey, s.buildPatientInsuranceQuery(targets))
	if err != nil {
		return nil, err
	}
	return dedupeByPatNumOrdinal(rows), nil
}

func (s *Selector) selectForSinglePatientAppointment(ctx context.Context, req PatientSelectRequest) ([]models.Appointment, error) {
	apiConfig, err := queryAPIConfig(req.ScraperConfig)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(req.PatNum) == "" {
		return nil, fmt.Errorf("patnum is required")
	}
	// Try future/today appointments first.
	rows, err := s.runQuery(ctx, apiConfig, req.OfficeKey, buildPatientQuery(req.PatNum, true))
	if err != nil {
		return nil, err
	}
	if len(rows) > 0 {
		return rows, nil
	}

	// No upcoming appointment — fall back to any scheduled appointment and use today as the date.
	rows, err = s.runQuery(ctx, apiConfig, req.OfficeKey, buildPatientQuery(req.PatNum, false))
	if err != nil {
		return nil, err
	}
	today := time.Now().Format("01-02-2006")
	for i := range rows {
		rows[i].AppointmentDate = today
	}
	return rows, nil
}

func (s *Selector) SelectForDay(ctx context.Context, req DaySelectRequest) ([]models.Appointment, error) {
	apiConfig, err := queryAPIConfig(req.ScraperConfig)
	if err != nil {
		return nil, err
	}

	query := s.buildDayQuery(req)
	rows, err := s.runQuery(ctx, apiConfig, req.OfficeKey, query)
	if err != nil {
		return nil, err
	}
	return dedupeByPatNumOrdinal(rows), nil
}

func (s *Selector) runQuery(ctx context.Context, apiConfig *QueryAPIConfig, officeKey string, query string) ([]models.Appointment, error) {
	body := map[string]any{
		"key":   officeKey,
		"query": query,
	}
	encodedBody, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("marshal appointment query: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, apiConfig.URL, bytes.NewReader(encodedBody))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Authorization", apiConfig.Token)
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json")

	resp, err := s.httpClient.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		if len(body) > 0 {
			return nil, fmt.Errorf("appointment query failed with status %s: %s", resp.Status, bytes.TrimSpace(body))
		}
		return nil, fmt.Errorf("appointment query failed with status %s", resp.Status)
	}

	rows, err := decodeAppointments(resp.Body)
	if err != nil {
		return nil, err
	}
	return rows, nil
}

func (s *Selector) buildAppointmentQuery(req SelectRequest) string {
	startDate, endDate := resolveSweepWindow(req.FutureRangeDays)
	statusFilterClause := ""
	if !req.IgnoreAppointmentStatusFilter {
		statusFilterClause = fmt.Sprintf(`
        AND NOT EXISTS (
            SELECT 1 FROM apptfield af
            WHERE af.AptNum = a.AptNum
            AND af.FieldName = 'HRDView'
            AND %s
        )`, terminalAppointmentFieldClause(req.RetryErrorsOnly))
	}

	// "*" means clearinghouse — query all carriers, no ElectID filter.
	carrierJoin := "JOIN carrier c ON c.CarrierNum=ip.CarrierNum"
	if !isWildcardPayerIDs(req.PayerIDs) {
		carrierJoin += fmt.Sprintf(" AND c.ElectID IN (%s)", quoteSQLList(req.PayerIDs))
	}

	return fmt.Sprintf(`SELECT a.AptNum AS aptNum, DATE_FORMAT(a.AptDateTime,'%%m-%%d-%%Y') AS appointmentDate,
        a.PatNum AS patNum, p.FName AS fName, p.LName AS lName, DATE_FORMAT(p.Birthdate,'%%m-%%d-%%Y')
        AS dob, p.Gender AS gender, sub_p.FName AS subFName, sub_p.LName AS subLName,
        DATE_FORMAT(sub_p.Birthdate,'%%m-%%d-%%Y') AS subDOB, sub_p.Gender AS subGender,
        ins.SubscriberID AS subscriberId,
        sub_p.SSN AS ssn, ip.GroupNum AS groupNum, ip.GroupName AS groupName,
        c.CarrierNum AS carrierNum, c.CarrierName AS CarrierName, c.ElectID AS payerId,
        pp.Ordinal AS ordinal, pp.Relationship AS relationship, ins.InsSubNum AS insSubNum,
        ins.PlanNum AS planNum,
        GROUP_CONCAT(DISTINCT pc.ProcCode ORDER BY pc.ProcCode SEPARATOR ', ')
        AS treatmentPlanProcCodes FROM appointment a
        JOIN patient p ON p.PatNum=a.PatNum
        JOIN patplan pp ON pp.PatNum=a.PatNum AND pp.Ordinal IN (1,2)
        JOIN inssub ins ON ins.InsSubNum=pp.InsSubNum
        JOIN patient sub_p ON sub_p.PatNum=ins.Subscriber
        JOIN insplan ip ON ip.PlanNum=ins.PlanNum
        %s
        LEFT JOIN procedurelog pl ON pl.PatNum=a.PatNum AND pl.ProcStatus='1'
        LEFT JOIN procedurecode pc ON pc.CodeNum=pl.CodeNum AND pc.ProcCode REGEXP '^D[0-9]+$'
        WHERE a.AptStatus=1
        AND DATE(a.AptDateTime) BETWEEN '%s'
            AND '%s'
        %s
        GROUP BY a.AptNum,a.AptDateTime,a.PatNum,p.FName,p.LName,p.Birthdate,p.Gender,
        sub_p.FName,sub_p.LName,sub_p.Birthdate,sub_p.Gender,ins.SubscriberID,sub_p.SSN,
        ip.GroupNum,ip.GroupName,c.CarrierNum,c.CarrierName,c.ElectID,pp.Ordinal,pp.Relationship,
        ins.InsSubNum,ins.PlanNum
        ORDER BY a.AptDateTime ASC
        LIMIT %d;`, carrierJoin, startDate, endDate, statusFilterClause, s.queryLimit)
}

// isWildcardPayerIDs returns true when payerIds is ["*"], signalling a clearinghouse
// that handles all payers and should not filter appointments by ElectID.
func isWildcardPayerIDs(ids []string) bool {
	return len(ids) == 1 && ids[0] == "*"
}

func (s *Selector) buildDayQuery(req DaySelectRequest) string {
	return fmt.Sprintf(`SELECT a.AptNum AS aptNum, DATE_FORMAT(a.AptDateTime,'%%m-%%d-%%Y') AS appointmentDate,
        a.PatNum AS patNum, p.FName AS fName, p.LName AS lName, DATE_FORMAT(p.Birthdate,'%%m-%%d-%%Y')
        AS dob, p.Gender AS gender, sub_p.FName AS subFName, sub_p.LName AS subLName,
        sub_p.zip AS subZip, DATE_FORMAT(sub_p.Birthdate,'%%m-%%d-%%Y') AS subDOB, sub_p.Gender AS subGender,
        ins.SubscriberID AS subscriberId,
        sub_p.SSN AS ssn, ip.GroupNum AS groupNum, ip.GroupName AS groupName,
        c.CarrierNum AS carrierNum, c.CarrierName AS CarrierName, c.ElectID AS payerId,
        pp.Ordinal AS ordinal, pp.Relationship AS relationship, ins.InsSubNum AS insSubNum,
        ins.PlanNum AS planNum,
        GROUP_CONCAT(DISTINCT pc.ProcCode ORDER BY pc.ProcCode SEPARATOR ', ')
        AS treatmentPlanProcCodes FROM appointment a
        JOIN patient p ON p.PatNum=a.PatNum
        JOIN patplan pp ON pp.PatNum=a.PatNum AND pp.Ordinal IN (1,2)
        JOIN inssub ins ON ins.InsSubNum=pp.InsSubNum
        JOIN patient sub_p ON sub_p.PatNum=ins.Subscriber
        JOIN insplan ip ON ip.PlanNum=ins.PlanNum
        JOIN carrier c ON c.CarrierNum=ip.CarrierNum
        LEFT JOIN procedurelog pl ON pl.PatNum=a.PatNum AND pl.ProcStatus='1'
        LEFT JOIN procedurecode pc ON pc.CodeNum=pl.CodeNum AND pc.ProcCode REGEXP '^D[0-9]+$'
        WHERE a.AptStatus=1
        AND DATE(a.AptDateTime) = CURRENT_DATE + INTERVAL %d DAY
        GROUP BY a.AptNum,a.AptDateTime,a.PatNum,p.FName,p.LName,p.Birthdate,p.Gender,
        sub_p.FName,sub_p.LName,sub_p.zip,sub_p.Birthdate,sub_p.Gender,ins.SubscriberID,sub_p.SSN,
        ip.GroupNum,ip.GroupName,c.CarrierNum,c.CarrierName,c.ElectID,pp.Ordinal,pp.Relationship,
        ins.InsSubNum,ins.PlanNum
        ORDER BY a.AptDateTime ASC
        LIMIT %d;`, req.AddDays, s.queryLimit)
}

func (s *Selector) buildPatientInsuranceQuery(targets []PatientTarget) string {
	targets = normalizePatientTargets(targets)
	limit := len(targets) * 2
	if limit <= 0 {
		limit = s.queryLimit
	}
	return fmt.Sprintf(`SELECT a.AptNum AS aptNum,
       DATE_FORMAT(a.AptDateTime,'%%m-%%d-%%Y') AS appointmentDate,
       p.PatNum AS patNum,
       p.FName AS fName, p.LName AS lName,
       DATE_FORMAT(p.Birthdate,'%%m-%%d-%%Y') AS dob,
       p.Gender AS gender,
       sub_p.FName AS subFName, sub_p.LName AS subLName,
       sub_p.Zip AS subZip,
       DATE_FORMAT(sub_p.Birthdate,'%%m-%%d-%%Y') AS subDOB,
       sub_p.Gender AS subGender,
       ins.SubscriberID AS subscriberId,
       sub_p.SSN AS ssn,
       ip.GroupNum AS groupNum, ip.GroupName AS groupName,
       c.CarrierNum AS carrierNum, c.CarrierName AS CarrierName,
       c.ElectID AS payerId,
       pp.Ordinal AS ordinal, pp.Relationship AS relationship,
       ins.InsSubNum AS insSubNum, ins.PlanNum AS planNum,
       GROUP_CONCAT(DISTINCT pc.ProcCode ORDER BY pc.ProcCode SEPARATOR ', ') AS treatmentPlanProcCodes
FROM (%s) target
JOIN patient p ON p.PatNum = target.PatNum
JOIN patplan pp ON pp.PatNum = p.PatNum AND pp.Ordinal IN (1,2)
JOIN inssub ins ON ins.InsSubNum = pp.InsSubNum
JOIN patient sub_p ON sub_p.PatNum = ins.Subscriber
JOIN insplan ip ON ip.PlanNum = ins.PlanNum
JOIN carrier c ON c.CarrierNum = ip.CarrierNum
LEFT JOIN appointment a ON target.AptNum <> '' AND a.AptNum = target.AptNum AND a.PatNum = p.PatNum
LEFT JOIN procedurelog pl ON pl.PatNum = p.PatNum AND pl.ProcStatus = '1'
LEFT JOIN procedurecode pc ON pc.CodeNum = pl.CodeNum AND pc.ProcCode REGEXP '^D[0-9]+$'
GROUP BY a.AptNum, a.AptDateTime, p.PatNum, p.FName, p.LName, p.Birthdate, p.Gender,
         sub_p.FName, sub_p.LName, sub_p.Zip, sub_p.Birthdate, sub_p.Gender,
         ins.SubscriberID, sub_p.SSN,
         ip.GroupNum, ip.GroupName, c.CarrierNum, c.CarrierName, c.ElectID,
         pp.Ordinal, pp.Relationship, ins.InsSubNum, ins.PlanNum
ORDER BY p.PatNum ASC, pp.Ordinal ASC
LIMIT %d;`, patientTargetsTable(targets), limit)
}

func buildPatientQuery(patNum string, futureOnly bool) string {
	dateFilter := ""
	if futureOnly {
		dateFilter = "AND DATE(a.AptDateTime) >= CURRENT_DATE"
	}
	return fmt.Sprintf(`SELECT a.AptNum AS aptNum, DATE_FORMAT(a.AptDateTime,'%%m-%%d-%%Y') AS appointmentDate,
        a.PatNum AS patNum, p.FName AS fName, p.LName AS lName, DATE_FORMAT(p.Birthdate,'%%m-%%d-%%Y')
        AS dob, p.Gender AS gender, sub_p.FName AS subFName, sub_p.LName AS subLName,
        sub_p.zip AS subZip, DATE_FORMAT(sub_p.Birthdate,'%%m-%%d-%%Y') AS subDOB, sub_p.Gender AS subGender,
        ins.SubscriberID AS subscriberId,
        sub_p.SSN AS ssn, ip.GroupNum AS groupNum, ip.GroupName AS groupName,
        c.CarrierNum AS carrierNum, c.CarrierName AS CarrierName, c.ElectID AS payerId,
        pp.Ordinal AS ordinal, pp.Relationship AS relationship, ins.InsSubNum AS insSubNum,
        ins.PlanNum AS planNum,
        GROUP_CONCAT(DISTINCT pc.ProcCode ORDER BY pc.ProcCode SEPARATOR ', ')
        AS treatmentPlanProcCodes FROM appointment a
        JOIN patient p ON p.PatNum=a.PatNum
        JOIN patplan pp ON pp.PatNum=a.PatNum AND pp.Ordinal IN (1,2)
        JOIN inssub ins ON ins.InsSubNum=pp.InsSubNum
        JOIN patient sub_p ON sub_p.PatNum=ins.Subscriber
        JOIN insplan ip ON ip.PlanNum=ins.PlanNum
        JOIN carrier c ON c.CarrierNum=ip.CarrierNum
        LEFT JOIN procedurelog pl ON pl.PatNum=a.PatNum AND pl.ProcStatus='1'
        LEFT JOIN procedurecode pc ON pc.CodeNum=pl.CodeNum AND pc.ProcCode REGEXP '^D[0-9]+$'
        WHERE a.AptStatus=1
        AND a.PatNum = '%s'
        %s
        GROUP BY a.AptNum,a.AptDateTime,a.PatNum,p.FName,p.LName,p.Birthdate,p.Gender,
        sub_p.FName,sub_p.LName,sub_p.zip,sub_p.Birthdate,sub_p.Gender,ins.SubscriberID,sub_p.SSN,
        ip.GroupNum,ip.GroupName,c.CarrierNum,c.CarrierName,c.ElectID,pp.Ordinal,pp.Relationship,
        ins.InsSubNum,ins.PlanNum
        ORDER BY a.AptDateTime ASC
        LIMIT 3;`, escapeSQLValue(strings.TrimSpace(patNum)), dateFilter)
}

func buildStatusFilterClause(retryErrorsOnly bool, ignoreAppointmentStatusFilter bool) string {
	if ignoreAppointmentStatusFilter {
		return ""
	}

	return fmt.Sprintf(`
        AND NOT EXISTS (
            SELECT 1 FROM apptfield af
            WHERE af.AptNum = a.AptNum
            AND af.FieldName = 'HRDView'
            AND %s
        )`, terminalAppointmentFieldClause(retryErrorsOnly))
}

func terminalAppointmentFieldClause(retryErrorsOnly bool) string {
	_ = retryErrorsOnly
	legacyStatuses := []string{"Verified", "Inactive", "Not Found", "Error", "V1"}
	return fmt.Sprintf("(af.FieldValue IN (%s) OR af.FieldValue LIKE 'NV1:%%')", quoteSQLList(legacyStatuses))
}

func queryAPIConfig(scraperConfig *models.ScraperConfig) (*QueryAPIConfig, error) {
	if scraperConfig == nil {
		return nil, fmt.Errorf("scraper config is missing")
	}

	raw, ok := scraperConfig.APIs["query"]
	if !ok {
		return nil, fmt.Errorf("query API config is missing")
	}

	apiMap, ok := raw.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("query API config has unexpected shape")
	}

	url, _ := apiMap["url"].(string)
	token, _ := apiMap["token"].(string)
	if url == "" || token == "" {
		return nil, fmt.Errorf("query API config requires url and token")
	}

	return &QueryAPIConfig{URL: url, Token: token}, nil
}

func decodeAppointments(reader io.Reader) ([]models.Appointment, error) {
	var raw json.RawMessage
	if err := json.NewDecoder(reader).Decode(&raw); err != nil {
		return nil, fmt.Errorf("decode appointment response: %w", err)
	}

	var wrapped queryResponse
	if err := json.Unmarshal(raw, &wrapped); err == nil {
		switch {
		case wrapped.ResultData != nil:
			return wrapped.ResultData, nil
		case wrapped.Data != nil:
			return wrapped.Data, nil
		case wrapped.Rows != nil:
			return wrapped.Rows, nil
		case wrapped.Results != nil:
			return wrapped.Results, nil
		}
	}

	var rows []models.Appointment
	if err := json.Unmarshal(raw, &rows); err != nil {
		return nil, fmt.Errorf("unexpected appointment response shape: %s", string(raw[:min(len(raw), 200)]))
	}

	return rows, nil
}

func dedupeByPatNumOrdinal(rows []models.Appointment) []models.Appointment {
	uniqueRows := make([]models.Appointment, 0, len(rows))
	seen := map[string]struct{}{}

	for _, row := range rows {
		if row.PatNum == "" {
			uniqueRows = append(uniqueRows, row)
			continue
		}
		ordinal := strings.TrimSpace(row.Ordinal)
		if ordinal == "" {
			ordinal = "1"
		}
		key := strings.Join([]string{
			strings.TrimSpace(row.PatNum),
			strings.TrimSpace(row.AptNum),
			strings.TrimSpace(row.InsSubNum),
			strings.TrimSpace(row.PlanNum),
			ordinal,
		}, "|")
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		uniqueRows = append(uniqueRows, row)
	}

	return uniqueRows
}

func resolveSweepWindow(futureRangeDays int) (string, string) {
	if futureRangeDays <= 0 {
		futureRangeDays = 10
	}

	start := startOfTodayUTC().AddDate(0, 0, 1)
	end := start.AddDate(0, 0, futureRangeDays)
	return formatDateOnly(start), formatDateOnly(end)
}

func targetDateForAddDays(now time.Time, addDays int) string {
	if addDays < 1 {
		addDays = 1
	}
	target := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location()).AddDate(0, 0, addDays)
	return formatDateOnly(target)
}

func startOfTodayUTC() time.Time {
	now := time.Now().UTC()
	return time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
}

func formatDateOnly(value time.Time) string {
	return value.Format("2006-01-02")
}

func quoteSQLList(values []string) string {
	quoted := make([]string, 0, len(values))
	for _, value := range values {
		trimmed := strings.TrimSpace(value)
		if trimmed == "" {
			continue
		}
		quoted = append(quoted, "'"+strings.ReplaceAll(trimmed, "'", "''")+"'")
	}
	return strings.Join(quoted, ",")
}

func escapeSQLValue(value string) string {
	return strings.ReplaceAll(value, "'", "''")
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

func patientTargetsTable(targets []PatientTarget) string {
	parts := make([]string, 0, len(targets))
	for _, target := range targets {
		parts = append(parts, fmt.Sprintf("SELECT '%s' AS PatNum, '%s' AS AptNum",
			escapeSQLValue(target.PatNum),
			escapeSQLValue(target.AptNum),
		))
	}
	if len(parts) == 0 {
		return "SELECT '' AS PatNum, '' AS AptNum"
	}
	return strings.Join(parts, " UNION ALL ")
}
