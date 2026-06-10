package officecodes

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"insurance-benefit-agent-go/internal/models"
)

const (
	cacheTTL        = 24 * time.Hour
	defaultTimeout  = 15 * time.Second
	cacheSourceName = "patcon-api"
)

var dCodePattern = regexp.MustCompile(`(?i)\bD\d{4}\b`)

type Service struct {
	mu         sync.RWMutex
	cache      map[string]*CachePayload
	httpClient *http.Client
}

type CachePayload struct {
	OfficeKey   string    `json:"officeKey"`
	FetchedAt   time.Time `json:"fetchedAt"`
	Source      string    `json:"source"`
	OfficeCodes []string  `json:"officeCodes"`
}

type PatconConfig struct {
	URL   string
	Token string
}

func New() *Service {
	return &Service{
		cache:      make(map[string]*CachePayload),
		httpClient: &http.Client{Timeout: defaultTimeout},
	}
}

func (s *Service) GetOfficeCodes(ctx context.Context, officeKey string, scraperConfig *models.ScraperConfig) ([]string, error) {
	s.mu.RLock()
	cached := s.cache[officeKey]
	s.mu.RUnlock()

	if cached != nil && !isExpired(cached.FetchedAt) {
		return cached.OfficeCodes, nil
	}

	officeCodes, err := s.fetchOfficeCodesFromAPI(ctx, officeKey, scraperConfig)
	if err != nil {
		if cached != nil && len(cached.OfficeCodes) > 0 {
			return cached.OfficeCodes, nil
		}
		return nil, err
	}

	payload := &CachePayload{
		OfficeKey:   officeKey,
		FetchedAt:   time.Now().UTC(),
		Source:      cacheSourceName,
		OfficeCodes: officeCodes,
	}
	s.mu.Lock()
	s.cache[officeKey] = payload
	s.mu.Unlock()

	return officeCodes, nil
}

func isExpired(fetchedAt time.Time) bool {
	if fetchedAt.IsZero() {
		return true
	}
	return time.Since(fetchedAt) > cacheTTL
}

func (s *Service) fetchOfficeCodesFromAPI(ctx context.Context, officeKey string, scraperConfig *models.ScraperConfig) ([]string, error) {
	patcon, err := extractPatconConfig(scraperConfig)
	if err != nil {
		return nil, err
	}

	url := fmt.Sprintf("%s/api/config/ins/form/licensekey/%s", strings.TrimRight(patcon.URL, "/"), officeKey)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", patcon.Token)

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		if len(body) > 0 {
			return nil, fmt.Errorf("office code fetch failed for office=%s: HTTP %s %s", officeKey, resp.Status, strings.TrimSpace(string(body)))
		}
		return nil, fmt.Errorf("office code fetch failed for office=%s: HTTP %s", officeKey, resp.Status)
	}

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read office code response: %w", err)
	}

	codes := parseOfficeCodes(raw)
	if len(codes) == 0 {
		return nil, fmt.Errorf("office code response did not contain any procedure codes")
	}
	return codes, nil
}

func extractPatconConfig(scraperConfig *models.ScraperConfig) (PatconConfig, error) {
	if scraperConfig == nil {
		return PatconConfig{}, fmt.Errorf("scraper config is missing")
	}
	raw, ok := scraperConfig.APIs["patcon"]
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

func parseOfficeCodes(raw []byte) []string {
	codes := map[string]struct{}{}

	var decoded any
	if err := json.Unmarshal(raw, &decoded); err == nil {
		collectCodes(decoded, codes)
	}
	if len(codes) == 0 {
		for _, match := range dCodePattern.FindAllString(string(raw), -1) {
			codes[strings.ToUpper(match)] = struct{}{}
		}
	}

	list := make([]string, 0, len(codes))
	for code := range codes {
		list = append(list, code)
	}
	slices.Sort(list)
	return list
}

func collectCodes(value any, codes map[string]struct{}) {
	switch v := value.(type) {
	case map[string]any:
		for key, nested := range v {
			if looksLikeCodeKey(key) {
				addCode(fmt.Sprint(nested), codes)
			}
			collectCodes(nested, codes)
		}
	case []any:
		for _, item := range v {
			collectCodes(item, codes)
		}
	case string:
		addCode(v, codes)
	}
}

func looksLikeCodeKey(key string) bool {
	key = strings.ToLower(strings.TrimSpace(key))
	return strings.Contains(key, "code")
}

func addCode(value string, codes map[string]struct{}) {
	for _, match := range dCodePattern.FindAllString(value, -1) {
		codes[strings.ToUpper(match)] = struct{}{}
	}
}
