package odetrans

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
	"strconv"
	"strings"
	"time"

	"insurance-benefit-agent-go/internal/models"
)

const (
	edi270QueryID         = "6a036e26efe592a1f5809f8b"
	etrans270QueryID      = "6a03b939efe592a1f5809f90"
	edi271QueryID         = "6a03bba626f6f4a2f06c5027"
	etrans271QueryID      = "6a03bc3426f6f4a2f06c502a"
	linkEtransPairQueryID = "6a03bd2aefe592a1f5809f94"
	defaultEtransUserNum  = 1
)

// PersistResult identifies the Open Dental rows created for a generated EDI pair.
type PersistResult struct {
	Msg270    int64
	Etrans270 int64
	Msg271    int64
	Etrans271 int64
}

// PersistPair inserts the generated 270/271 pair into Open Dental through
// PatCon saved queries, then links the 270 etrans row to the 271 response.
func PersistPair(ctx context.Context, scraperConfig *models.ScraperConfig, officeKey string, appt models.Appointment, pair Pair, now time.Time) (PersistResult, error) {
	if strings.TrimSpace(pair.Request270) == "" || strings.TrimSpace(pair.Response271) == "" {
		return PersistResult{}, fmt.Errorf("edi pair is empty")
	}
	api, err := persistQueryAPIConfig(scraperConfig)
	if err != nil {
		return PersistResult{}, err
	}
	if now.IsZero() {
		now = time.Now()
	}
	datetimeTrans := now.Format("2006-01-02 15:04:05")
	common := map[string]any{
		"datetime_trans":    datetimeTrans,
		"clearinghouse_num": syntheticClearingHouseNum,
		"carrier_num":       sqlInt(appt.CarrierNum),
		"pat_num":           sqlInt(appt.PatNum),
		"plan_num":          sqlInt(appt.PlanNum),
		"ins_sub_num":       sqlInt(appt.InsSubNum),
		"user_num":          defaultEtransUserNum,
	}

	var result PersistResult
	result.Msg270, err = api.executeInt(ctx, officeKey, edi270QueryID, map[string]any{"edi270": pair.Request270}, "msg270")
	if err != nil {
		return result, fmt.Errorf("insert edi270 message: %w", err)
	}
	log.Printf("[ODEtrans] saved query edi270 ReturnId=%d", result.Msg270)
	params270 := cloneParams(common)
	params270["msg270"] = result.Msg270
	result.Etrans270, err = api.executeInt(ctx, officeKey, etrans270QueryID, params270, "etrans270")
	if err != nil {
		return result, fmt.Errorf("insert etrans270: %w", err)
	}
	log.Printf("[ODEtrans] saved query etrans270 ReturnId=%d", result.Etrans270)

	result.Msg271, err = api.executeInt(ctx, officeKey, edi271QueryID, map[string]any{"edi271": pair.Response271}, "msg271")
	if err != nil {
		return result, fmt.Errorf("insert edi271 message: %w", err)
	}
	log.Printf("[ODEtrans] saved query edi271 ReturnId=%d", result.Msg271)
	params271 := cloneParams(common)
	params271["msg271"] = result.Msg271
	result.Etrans271, err = api.executeInt(ctx, officeKey, etrans271QueryID, params271, "etrans271")
	if err != nil {
		return result, fmt.Errorf("insert etrans271: %w", err)
	}
	log.Printf("[ODEtrans] saved query etrans271 ReturnId=%d", result.Etrans271)

	if _, err := api.execute(ctx, officeKey, linkEtransPairQueryID, map[string]any{
		"etrans271": result.Etrans271,
		"etrans270": result.Etrans270,
	}); err != nil {
		return result, fmt.Errorf("link etrans 270/271: %w", err)
	}
	log.Printf("[ODEtrans] saved query link etrans270=%d etrans271=%d", result.Etrans270, result.Etrans271)
	return result, nil
}

type persistQueryAPI struct {
	ExecuteBaseURL string
	Token          string
	Client         *http.Client
}

func persistQueryAPIConfig(scraperConfig *models.ScraperConfig) (persistQueryAPI, error) {
	if scraperConfig == nil {
		return persistQueryAPI{}, fmt.Errorf("scraper config is missing")
	}
	raw, ok := scraperConfig.APIs["query"]
	if !ok {
		return persistQueryAPI{}, fmt.Errorf("query API config is missing")
	}
	apiMap, ok := raw.(map[string]any)
	if !ok {
		return persistQueryAPI{}, fmt.Errorf("query API config has unexpected shape")
	}
	rawURL, _ := apiMap["url"].(string)
	token, _ := apiMap["token"].(string)
	if strings.TrimSpace(rawURL) == "" || strings.TrimSpace(token) == "" {
		return persistQueryAPI{}, fmt.Errorf("query API config requires url and token")
	}
	return persistQueryAPI{
		ExecuteBaseURL: savedQueryBaseURL(rawURL),
		Token:          strings.TrimSpace(token),
		Client:         &http.Client{Timeout: 30 * time.Second},
	}, nil
}

func savedQueryBaseURL(rawURL string) string {
	rawURL = strings.TrimSpace(rawURL)
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return strings.TrimRight(rawURL, "/") + "/api/query/execute"
	}
	parsed.Path = strings.TrimSuffix(parsed.Path, "/api/run/query")
	parsed.Path = path.Join(parsed.Path, "/api/query/execute")
	parsed.RawQuery = ""
	parsed.Fragment = ""
	return parsed.String()
}

func (api persistQueryAPI) executeInt(ctx context.Context, officeKey, queryID string, params map[string]any, preferredKeys ...string) (int64, error) {
	raw, err := api.execute(ctx, officeKey, queryID, params)
	if err != nil {
		return 0, err
	}
	value, ok := extractInt(raw, preferredKeys...)
	if !ok {
		return 0, fmt.Errorf("query %s response did not include inserted id: %s", queryID, previewJSON(raw))
	}
	return value, nil
}

func (api persistQueryAPI) execute(ctx context.Context, officeKey, queryID string, params map[string]any) (json.RawMessage, error) {
	body := cloneParams(params)
	body["queryId"] = queryID
	body["licenseKey"] = officeKey
	encodedBody, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("marshal saved query request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(api.ExecuteBaseURL, "/")+"/"+queryID, bytes.NewReader(encodedBody))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", api.Token)
	req.Header.Set("token", api.Token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	client := api.Client
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		if len(body) > 0 {
			return nil, fmt.Errorf("saved query %s failed with status %s: %s", queryID, resp.Status, bytes.TrimSpace(body))
		}
		return nil, fmt.Errorf("saved query %s failed with status %s", queryID, resp.Status)
	}
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 1024*1024))
	if err != nil {
		return nil, fmt.Errorf("read saved query response: %w", err)
	}
	var raw json.RawMessage
	if err := json.Unmarshal(respBody, &raw); err != nil {
		return nil, fmt.Errorf("decode saved query response: %w body=%q", err, previewText(respBody))
	}
	return raw, nil
}

func cloneParams(params map[string]any) map[string]any {
	out := make(map[string]any, len(params)+2)
	for key, value := range params {
		out[key] = value
	}
	return out
}

func extractInt(raw json.RawMessage, preferredKeys ...string) (int64, bool) {
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return 0, false
	}
	keys := append([]string{}, preferredKeys...)
	keys = append(keys, "ReturnId", "returnId", "insertId", "insert_id", "id", "last_insert_id", "LAST_INSERT_ID()")
	return extractIntFromValue(value, keys)
}

func extractIntFromValue(value any, keys []string) (int64, bool) {
	switch typed := value.(type) {
	case float64:
		return int64(typed), true
	case string:
		n, err := strconv.ParseInt(strings.TrimSpace(typed), 10, 64)
		return n, err == nil
	case []any:
		for _, item := range typed {
			if n, ok := extractIntFromValue(item, keys); ok {
				return n, true
			}
		}
	case map[string]any:
		for _, key := range keys {
			for actual, item := range typed {
				if strings.EqualFold(actual, key) {
					if n, ok := extractIntFromValue(item, keys); ok {
						return n, true
					}
				}
			}
		}
		for _, key := range []string{"data", "ResultData", "rows", "results"} {
			if item, ok := typed[key]; ok {
				if n, ok := extractIntFromValue(item, keys); ok {
					return n, true
				}
			}
		}
	}
	return 0, false
}

func previewJSON(raw json.RawMessage) string {
	return previewText(raw)
}

func previewText(raw []byte) string {
	text := string(bytes.TrimSpace(raw))
	if len(text) > 300 {
		return text[:300] + "..."
	}
	return text
}
