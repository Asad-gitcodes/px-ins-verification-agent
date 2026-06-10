package controlplane

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"insurance-benefit-agent-go/internal/config"
	"insurance-benefit-agent-go/internal/models"
)

const (
	trackingStartPath = "/api/config/start/qispayor/tracking"
	trackingEndPath   = "/api/config/end/qispayor/tracking"
)

type Client struct {
	cfg           *config.Config
	httpClient    *http.Client
	credentialsMu sync.RWMutex
	patconURL     string
	patconToken   string
}

func NewClient(cfg *config.Config) *Client {
	return &Client{
		cfg:         cfg,
		httpClient:  &http.Client{Timeout: 60 * time.Second},
		patconURL:   cfg.Bootstrap.Patcon.URL,
		patconToken: cfg.Bootstrap.Patcon.Token,
	}
}

// FetchServerConfig retrieves the full office config (payers, credentials, MFA)
// from the patcon endpoint.
func (c *Client) FetchServerConfig(ctx context.Context) (*models.ServerConfig, error) {
	url := fmt.Sprintf("%s/api/config/insSched/details/%s",
		strings.TrimRight(c.cfg.Bootstrap.Patcon.URL, "/"),
		c.cfg.OfficeKey,
	)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", c.cfg.Bootstrap.Patcon.Token)
	req.Header.Set("Accept", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		if len(body) > 0 {
			return nil, fmt.Errorf("fetch server config failed with status %s: %s", resp.Status, bytes.TrimSpace(body))
		}
		return nil, fmt.Errorf("fetch server config failed with status %s", resp.Status)
	}

	var sc models.ServerConfig
	if err := json.NewDecoder(resp.Body).Decode(&sc); err != nil {
		return nil, fmt.Errorf("decode server config: %w", err)
	}
	c.ApplyServerConfig(&sc)

	return &sc, nil
}

// ApplyServerConfig switches follow-up PatCon calls to the operational
// MAIN_DOMAIN/MAIN_API_TOKEN values returned by bootstrap config.
func (c *Client) ApplyServerConfig(sc *models.ServerConfig) {
	if sc == nil {
		return
	}
	c.SetPatconCredentials(sc.MainDomain, sc.MainAPIToken)
}

func (c *Client) SetPatconCredentials(rawURL, token string) {
	rawURL = strings.TrimSpace(rawURL)
	token = strings.TrimSpace(token)
	if rawURL == "" && token == "" {
		return
	}

	c.credentialsMu.Lock()
	defer c.credentialsMu.Unlock()
	if rawURL != "" {
		c.patconURL = rawURL
	}
	if token != "" {
		c.patconToken = token
	}
}

func (c *Client) patconCredentials() (baseURL string, token string) {
	c.credentialsMu.RLock()
	defer c.credentialsMu.RUnlock()
	return strings.TrimRight(c.patconURL, "/"), c.patconToken
}

// StartPayerTracking calls the start tracking endpoint before a payer run begins.
// Returns the tracking record ID to be passed to EndPayerTracking.
func (c *Client) StartPayerTracking(ctx context.Context, userID int, payerID string) (string, error) {
	base, token := c.patconCredentials()
	url := fmt.Sprintf("%s%s/%d/%s", base, trackingStartPath, userID, payerID)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", token)
	req.Header.Set("Accept", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("start tracking failed status=%s body=%s", resp.Status, bytes.TrimSpace(b))
	}

	// Server returns the tracking ID as a plain JSON string.
	var id string
	if err := json.Unmarshal(b, &id); err != nil || id == "" {
		return "", fmt.Errorf("start tracking: could not parse tracking id from response: %s", bytes.TrimSpace(b))
	}

	return id, nil
}

// EndPayerTracking calls the end tracking endpoint after a payer run completes.
func (c *Client) EndPayerTracking(ctx context.Context, userID int, trackingID, payerID, status string, report any) error {
	base, token := c.patconCredentials()
	url := fmt.Sprintf("%s%s/%d/%s/%s", base, trackingEndPath, userID, payerID, trackingID)

	body, err := json.Marshal(map[string]any{
		"status": status,
		"report": report,
	})
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", token)
	req.Header.Set("Accept", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("end tracking failed status=%s body=%s", resp.Status, bytes.TrimSpace(b))
	}

	return nil
}
