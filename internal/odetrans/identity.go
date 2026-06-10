package odetrans

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

const defaultDentalTaxonomy = "1223G0001X"

// ProviderIdentity is the provider/office identity used in the 270 provider loop.
type ProviderIdentity struct {
	FirstName string `json:"provFName,omitempty"`
	LastName  string `json:"provLName,omitempty"`
	TaxID     string `json:"provTaxId,omitempty"`
	NPI       string `json:"provNPI,omitempty"`
	Taxonomy  string `json:"taxonomy,omitempty"`
}

// PracticeIdentity is the practice address used in the 270 provider loop.
type PracticeIdentity struct {
	Address string `json:"address,omitempty"`
	City    string `json:"city,omitempty"`
	State   string `json:"state,omitempty"`
	Zip     string `json:"zip,omitempty"`
}

// OfficeIdentity groups provider and practice information for generated EDI.
type OfficeIdentity struct {
	Provider ProviderIdentity `json:"provider,omitempty"`
	Practice PracticeIdentity `json:"practice,omitempty"`
}

// ResolveIdentity returns provided identity values, querying OD through PatCon
// for missing provider/practice fields when scraper config is available.
func ResolveIdentity(ctx context.Context, scraperConfig *models.ScraperConfig, officeKey string, provided OfficeIdentity) (OfficeIdentity, error) {
	out := OfficeIdentity{
		Provider: normalizeProviderIdentity(provided.Provider),
		Practice: normalizePracticeIdentity(provided.Practice),
	}
	if identityComplete(out) {
		return out, nil
	}
	api, err := identityQueryAPIConfig(scraperConfig)
	if err != nil {
		out.Provider = normalizeProviderIdentity(out.Provider)
		out.Practice = normalizePracticeIdentity(out.Practice)
		return out, err
	}
	if !providerComplete(out.Provider) {
		provider, err := queryProviderIdentity(ctx, api, officeKey)
		if err != nil {
			return out, err
		}
		out.Provider = mergeProviderIdentity(out.Provider, provider)
	}
	if !practiceComplete(out.Practice) {
		practice, err := queryPracticeIdentity(ctx, api, officeKey)
		if err != nil {
			return out, err
		}
		out.Practice = mergePracticeIdentity(out.Practice, practice)
	}
	out.Provider = normalizeProviderIdentity(out.Provider)
	out.Practice = normalizePracticeIdentity(out.Practice)
	return out, nil
}

type identityQueryAPI struct {
	URL    string
	Token  string
	Client *http.Client
}

func identityQueryAPIConfig(scraperConfig *models.ScraperConfig) (identityQueryAPI, error) {
	if scraperConfig == nil {
		return identityQueryAPI{}, fmt.Errorf("scraper config is missing")
	}
	raw, ok := scraperConfig.APIs["query"]
	if !ok {
		return identityQueryAPI{}, fmt.Errorf("query API config is missing")
	}
	apiMap, ok := raw.(map[string]any)
	if !ok {
		return identityQueryAPI{}, fmt.Errorf("query API config has unexpected shape")
	}
	url, _ := apiMap["url"].(string)
	token, _ := apiMap["token"].(string)
	if strings.TrimSpace(url) == "" || strings.TrimSpace(token) == "" {
		return identityQueryAPI{}, fmt.Errorf("query API config requires url and token")
	}
	return identityQueryAPI{
		URL:    strings.TrimSpace(url),
		Token:  strings.TrimSpace(token),
		Client: &http.Client{Timeout: 30 * time.Second},
	}, nil
}

func queryProviderIdentity(ctx context.Context, api identityQueryAPI, officeKey string) (ProviderIdentity, error) {
	rows, err := runIdentityQuery[providerRow](ctx, api, officeKey, `SELECT LName, FName, SSN, NationalProvID, TaxonomyCodeOverride FROM provider WHERE ItemOrder='1' LIMIT 1`)
	if err != nil {
		return ProviderIdentity{}, err
	}
	if len(rows) == 0 {
		return ProviderIdentity{}, nil
	}
	row := rows[0]
	return ProviderIdentity{
		FirstName: row.FName,
		LastName:  row.LName,
		TaxID:     row.SSN,
		NPI:       row.NationalProvID,
		Taxonomy:  row.TaxonomyCodeOverride,
	}, nil
}

func queryPracticeIdentity(ctx context.Context, api identityQueryAPI, officeKey string) (PracticeIdentity, error) {
	rows, err := runIdentityQuery[preferenceRow](ctx, api, officeKey, `SELECT PrefName, ValueString FROM preference WHERE PrefName IN ('PracticeAddress','PracticeCity','PracticeST','PracticeZip') LIMIT 10`)
	if err != nil {
		return PracticeIdentity{}, err
	}
	var practice PracticeIdentity
	for _, row := range rows {
		switch strings.TrimSpace(row.PrefName) {
		case "PracticeAddress":
			practice.Address = row.ValueString
		case "PracticeCity":
			practice.City = row.ValueString
		case "PracticeST":
			practice.State = row.ValueString
		case "PracticeZip":
			practice.Zip = row.ValueString
		}
	}
	return practice, nil
}

type providerRow struct {
	LName                string `json:"LName"`
	FName                string `json:"FName"`
	SSN                  string `json:"SSN"`
	NationalProvID       string `json:"NationalProvID"`
	TaxonomyCodeOverride string `json:"TaxonomyCodeOverride"`
}

type preferenceRow struct {
	PrefName    string `json:"PrefName"`
	ValueString string `json:"ValueString"`
}

type identityQueryResponse[T any] struct {
	ResultData []T `json:"ResultData"`
	Data       []T `json:"data"`
	Rows       []T `json:"rows"`
	Results    []T `json:"results"`
}

func runIdentityQuery[T any](ctx context.Context, api identityQueryAPI, officeKey, query string) ([]T, error) {
	body := map[string]any{
		"key":   officeKey,
		"query": query,
	}
	encodedBody, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("marshal identity query: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, api.URL, bytes.NewReader(encodedBody))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", api.Token)
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
			return nil, fmt.Errorf("identity query failed with status %s: %s", resp.Status, bytes.TrimSpace(body))
		}
		return nil, fmt.Errorf("identity query failed with status %s", resp.Status)
	}
	var raw json.RawMessage
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return nil, fmt.Errorf("decode identity response: %w", err)
	}
	var wrapped identityQueryResponse[T]
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
	var rows []T
	if err := json.Unmarshal(raw, &rows); err != nil {
		return nil, fmt.Errorf("unexpected identity response shape: %s", string(raw[:min(len(raw), 200)]))
	}
	return rows, nil
}

func normalizeProviderIdentity(provider ProviderIdentity) ProviderIdentity {
	provider.FirstName = strings.TrimSpace(provider.FirstName)
	provider.LastName = strings.TrimSpace(provider.LastName)
	provider.TaxID = digitsOnly(provider.TaxID)
	provider.NPI = digitsOnly(provider.NPI)
	provider.Taxonomy = strings.ToUpper(strings.TrimSpace(provider.Taxonomy))
	if provider.Taxonomy == "" {
		provider.Taxonomy = defaultDentalTaxonomy
	}
	return provider
}

func normalizePracticeIdentity(practice PracticeIdentity) PracticeIdentity {
	practice.Address = strings.TrimSpace(practice.Address)
	practice.City = strings.TrimSpace(practice.City)
	practice.State = strings.ToUpper(strings.TrimSpace(practice.State))
	practice.Zip = digitsOnly(practice.Zip)
	return practice
}

func identityComplete(identity OfficeIdentity) bool {
	return providerComplete(identity.Provider) && practiceComplete(identity.Practice)
}

func providerComplete(provider ProviderIdentity) bool {
	provider = normalizeProviderIdentity(provider)
	return provider.FirstName != "" && provider.LastName != "" && provider.TaxID != "" && provider.NPI != "" && provider.Taxonomy != ""
}

func practiceComplete(practice PracticeIdentity) bool {
	practice = normalizePracticeIdentity(practice)
	return practice.Address != "" && practice.City != "" && practice.State != "" && practice.Zip != ""
}

func mergeProviderIdentity(base, fallback ProviderIdentity) ProviderIdentity {
	base.FirstName = firstNonEmpty(base.FirstName, fallback.FirstName)
	base.LastName = firstNonEmpty(base.LastName, fallback.LastName)
	base.TaxID = firstNonEmpty(base.TaxID, fallback.TaxID)
	base.NPI = firstNonEmpty(base.NPI, fallback.NPI)
	base.Taxonomy = firstNonEmpty(base.Taxonomy, fallback.Taxonomy)
	return base
}

func mergePracticeIdentity(base, fallback PracticeIdentity) PracticeIdentity {
	base.Address = firstNonEmpty(base.Address, fallback.Address)
	base.City = firstNonEmpty(base.City, fallback.City)
	base.State = firstNonEmpty(base.State, fallback.State)
	base.Zip = firstNonEmpty(base.Zip, fallback.Zip)
	return base
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
