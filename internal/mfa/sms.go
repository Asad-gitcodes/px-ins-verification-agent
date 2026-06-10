package mfa

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"regexp"
	"time"

	"insurance-benefit-agent-go/internal/models"
)

const smsQuery = "Select * from smsfrommobile order by 1 desc limit 1;"

var sixDigitSMS = regexp.MustCompile(`\b(\d{6})\b`)

type SMSConfig struct {
	QueryURL       string
	Token          string
	OfficeKey      string
	TimeoutMS      int
	PollIntervalMS int
}

// SMSConfigFromScraperConfig extracts the query API URL/token from ScraperConfig.APIs["query"].
func SMSConfigFromScraperConfig(scraperConfig *models.ScraperConfig, officeKey string) (*SMSConfig, error) {
	if scraperConfig == nil {
		return nil, fmt.Errorf("scraper config is missing")
	}
	raw, ok := scraperConfig.APIs["query"]
	if !ok {
		return nil, fmt.Errorf("query API config not found in scraper config")
	}
	apiMap, ok := raw.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("query API config has unexpected shape")
	}
	queryURL, _ := apiMap["url"].(string)
	token, _ := apiMap["token"].(string)
	if queryURL == "" || token == "" {
		return nil, fmt.Errorf("query API config requires url and token")
	}
	return &SMSConfig{
		QueryURL:  queryURL,
		Token:     token,
		OfficeKey: officeKey,
	}, nil
}

// GetSmsCode polls the query API for a fresh 6-digit SMS OTP received after `after`.
func GetSmsCode(cfg SMSConfig, after time.Time) (string, error) {
	return GetSmsCodeByPattern(cfg, after, sixDigitSMS)
}

// GetSmsCodeByPattern polls like GetSmsCode but uses a caller-supplied regexp.
// The regexp must have exactly one capture group containing the code.
func GetSmsCodeByPattern(cfg SMSConfig, after time.Time, pattern *regexp.Regexp) (string, error) {
	if cfg.QueryURL == "" {
		return "", fmt.Errorf("SMS query URL is required")
	}
	timeout := durationFromMillis(cfg.TimeoutMS, 60*time.Second)
	pollInterval := durationFromMillis(cfg.PollIntervalMS, 3*time.Second)
	deadline := time.Now().Add(timeout)

	httpClient := &http.Client{Timeout: 10 * time.Second}

	for {
		code, err := pollSMSOnce(httpClient, cfg, after, pattern)
		if err != nil {
			return "", err
		}
		if code != "" {
			return code, nil
		}
		if time.Now().After(deadline) {
			return "", fmt.Errorf("timed out waiting for SMS OTP code")
		}
		time.Sleep(pollInterval)
	}
}

func pollSMSOnce(httpClient *http.Client, cfg SMSConfig, after time.Time, pattern *regexp.Regexp) (string, error) {
	body, err := json.Marshal(map[string]any{
		"key":   cfg.OfficeKey,
		"query": smsQuery,
	})
	if err != nil {
		return "", fmt.Errorf("marshal SMS query: %w", err)
	}

	req, err := http.NewRequest(http.MethodPost, cfg.QueryURL, bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("build SMS query request: %w", err)
	}
	req.Header.Set("Authorization", cfg.Token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("SMS query request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return "", fmt.Errorf("SMS query returned status %s: %s", resp.Status, b)
	}

	var raw json.RawMessage
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return "", fmt.Errorf("decode SMS query response: %w", err)
	}
	rows := decodeSMSRows(raw)
	if len(rows) == 0 {
		return "", nil
	}

	row := rows[0]
	received, err := parseSMSDateTime(string(row.DateTimeReceived))
	if err != nil {
		received, err = time.Parse("2006-01-02 15:04:05", row.DateTimeReceived)
		if err != nil {
			log.Printf("[MFA] SMS timestamp unparseable: %q", row.DateTimeReceived)
			return "", nil
		}
	}
	if !received.After(after) {
		return "", nil
	}

	match := pattern.FindStringSubmatch(row.MsgText)
	if len(match) < 2 {
		return "", nil
	}
	log.Printf("[MFA] SMS OTP found, received=%s", received.Format(time.RFC3339))
	return match[1], nil
}

type smsRow struct {
	DateTimeReceived string `json:"DateTimeReceived"`
	MsgText          string `json:"MsgText"`
}

type smsQueryResponse struct {
	ResultData []smsRow `json:"ResultData"`
	Data       []smsRow `json:"data"`
	Rows       []smsRow `json:"rows"`
	Results    []smsRow `json:"results"`
}

func decodeSMSRows(raw json.RawMessage) []smsRow {
	var wrapped smsQueryResponse
	if err := json.Unmarshal(raw, &wrapped); err == nil {
		switch {
		case len(wrapped.ResultData) > 0:
			return wrapped.ResultData
		case len(wrapped.Data) > 0:
			return wrapped.Data
		case len(wrapped.Rows) > 0:
			return wrapped.Rows
		case len(wrapped.Results) > 0:
			return wrapped.Results
		}
	}
	var rows []smsRow
	_ = json.Unmarshal(raw, &rows)
	return rows
}

func parseSMSDateTime(raw string) (time.Time, error) {
	layouts := []string{
		time.RFC3339,
		"2006-01-02T15:04:05",
		"2006-01-02 15:04:05",
	}

	loc := time.Local
	var lastErr error

	for _, layout := range layouts {
		var t time.Time
		var err error

		if layout == time.RFC3339 {
			t, err = time.Parse(layout, raw)
		} else {
			t, err = time.ParseInLocation(layout, raw, loc)
		}
		if err == nil {
			return t, nil
		}
		lastErr = err
	}

	return time.Time{}, fmt.Errorf("unsupported datetime %q: %w", raw, lastErr)
}
