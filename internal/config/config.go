package config

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
)

//go:embed defaults.json
var embeddedDefaults []byte

type Config struct {
	path string `json:"-"`

	OfficeKey    string          `json:"officeKey"`
	Bootstrap    BootstrapConfig `json:"bootstrapUrl"`
	API          APIConfig       `json:"api"`
	SnapshotPath    string `json:"-"` // set via --snapshot CLI flag; bypasses PatCon
	RunOnceAddDays  int    `json:"-"` // set via --add-days CLI flag; controls appointment lookahead

	// Local holds settings from the optional agent.local.json file.
	// It is loaded alongside the main config and is never written back.
	Local *LocalConfig `json:"-"`

	Sweep   SweepConfig   `json:"sweep"`
	PDF     PDFConfig     `json:"pdf,omitempty"`
	Updates UpdateConfig  `json:"updates,omitempty"`
	Testing TestingConfig `json:"testing"`
}

type BootstrapConfig struct {
	Patcon PatconConfig `json:"patcon"`
}

type PatconConfig struct {
	URL   string `json:"url"`
	Token string `json:"token"`
}

type APIConfig struct {
	Enabled           bool   `json:"enabled"`
	ListenAddr        string `json:"listenAddr"`
	BearerToken       string `json:"bearerToken"`
	ReadTimeoutMS     int    `json:"readTimeoutMs"`
	ShutdownTimeoutMS int    `json:"shutdownTimeoutMs"`
}

type SweepConfig struct {
	IntervalMS int `json:"intervalMs"`
	QueryLimit int `json:"queryLimit"`
}

// PDFConfig controls production PDF generation/upload behavior.
type PDFConfig struct {
	// Enabled forces PDF generation/upload on or off from agent.config.json.
	// When omitted, the server insPDFGenerate value decides.
	Enabled *bool `json:"enabled,omitempty"`
}

// UpdateConfig controls agent self-update checks.
// source "local"  — reads manifest.json + binary from localDir on disk.
// source "patcon" — uses bootstrapUrl.patcon.url + token; no extra URLs needed.
type UpdateConfig struct {
	Enabled              bool   `json:"enabled,omitempty"`
	Source               string `json:"source,omitempty"` // "local" or "patcon"
	LocalDir             string `json:"localDir,omitempty"`
	Channel              string `json:"channel,omitempty"` // stable, beta, rc
	CheckIntervalMinutes int    `json:"checkIntervalMinutes,omitempty"`
}

// TestingConfig controls write-back actions and diagnostic artifact output.
// Omitting the "testing" key entirely (production) enables all write-backs
// and suppresses debug artifacts.
//
// For a local test run set these in agent.config.json under "testing":
//
//	"skipTracking":    true  — skip StartPayerTracking / EndPayerTracking API calls
//	"skipApptField":   true  — skip HRDView apptfield write to OD
//	"localPdfOnly":    true  — write PDF to ./pdfs/ on disk; do not upload to API
//	"allAppointments": true  — ignore apptfield filter; fetch all appointments
//	"maxAppointments": N     — cap appointments per payer to N (0 = no cap)
//	"apptRangeDays":   N     — override appointment range days
type TestingConfig struct {
	WritePDF            *bool `json:"writePdf"`
	UpdateApptField     *bool `json:"updateApptField"`
	WriteDebugArtifacts *bool `json:"writeDebugArtifacts"`
	MaxAppointments     *int  `json:"maxAppointments"`
	ApptRangeDays       *int  `json:"apptRangeDays"`
	SkipTracking        *bool `json:"skipTracking"`
	SkipApptField       *bool `json:"skipApptField"`
	LocalPDFOnly        *bool `json:"localPdfOnly"`
	AllAppointments     *bool `json:"allAppointments"`
	Headless            *bool `json:"headless"`
}

// ShouldUpdateApptField returns true unless explicitly disabled via updateApptField or skipApptField.
func (t TestingConfig) ShouldUpdateApptField() bool {
	if t.SkipApptField != nil && *t.SkipApptField {
		return false
	}
	return t.UpdateApptField == nil || *t.UpdateApptField
}

// ShouldSkipTracking returns true when tracking API calls should be suppressed.
func (t TestingConfig) ShouldSkipTracking() bool {
	return t.SkipTracking != nil && *t.SkipTracking
}

// ShouldUseAllAppointments returns true when the apptfield status filter should be ignored.
func (t TestingConfig) ShouldUseAllAppointments() bool {
	return t.AllAppointments != nil && *t.AllAppointments
}

// ShouldUsePDFLocalOnly returns true when PDFs should be written to disk instead of uploaded.
func (t TestingConfig) ShouldUsePDFLocalOnly() bool {
	return t.LocalPDFOnly != nil && *t.LocalPDFOnly
}

// IsHeadless returns the testing headless override, falling back to defaultVal.
func (t TestingConfig) IsHeadless(defaultVal bool) bool {
	if t.Headless == nil {
		return defaultVal
	}
	return *t.Headless
}

// ShouldWriteDebugArtifacts returns true only when explicitly enabled.
func (t TestingConfig) ShouldWriteDebugArtifacts() bool {
	return t.WriteDebugArtifacts != nil && *t.WriteDebugArtifacts
}

func (c *Config) Path() string {
	return c.path
}

// loadDefaults unmarshals the embedded defaults.json into a fresh Config.
func loadDefaults(path string) (*Config, error) {
	var cfg Config
	if err := json.Unmarshal(embeddedDefaults, &cfg); err != nil {
		return nil, fmt.Errorf("parse embedded defaults: %w", err)
	}
	cfg.path = path
	return &cfg, nil
}

// Default returns a Config populated entirely from embedded defaults.
// Required fields (officeKey, patcon URL/token) are supplied via CLI flags after this call.
func Default(path string) (*Config, error) {
	cfg, err := loadDefaults(path)
	if err != nil {
		return nil, err
	}
	local, err := loadLocalConfig(path)
	if err != nil {
		return nil, err
	}
	cfg.Local = local
	applyLocalTesting(cfg, local)
	return cfg, nil
}

// Load reads an optional config file and overlays it on top of embedded defaults.
// Required fields (officeKey, patcon URL/token) are not expected in the file —
// they are always supplied via CLI flags.
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}

	cfg, err := loadDefaults(path)
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(raw, cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}

	local, err := loadLocalConfig(path)
	if err != nil {
		return nil, err
	}
	cfg.Local = local
	applyLocalTesting(cfg, local)
	return cfg, nil
}

func applyLocalTesting(cfg *Config, local *LocalConfig) {
	if cfg == nil || local == nil {
		return
	}
	mergeTestingConfig(&cfg.Testing, local.Testing)
}

func mergeTestingConfig(dst *TestingConfig, src TestingConfig) {
	if dst == nil {
		return
	}
	if src.WritePDF != nil {
		dst.WritePDF = src.WritePDF
	}
	if src.UpdateApptField != nil {
		dst.UpdateApptField = src.UpdateApptField
	}
	if src.WriteDebugArtifacts != nil {
		dst.WriteDebugArtifacts = src.WriteDebugArtifacts
	}
	if src.MaxAppointments != nil {
		dst.MaxAppointments = src.MaxAppointments
	}
	if src.ApptRangeDays != nil {
		dst.ApptRangeDays = src.ApptRangeDays
	}
	if src.SkipTracking != nil {
		dst.SkipTracking = src.SkipTracking
	}
	if src.SkipApptField != nil {
		dst.SkipApptField = src.SkipApptField
	}
	if src.LocalPDFOnly != nil {
		dst.LocalPDFOnly = src.LocalPDFOnly
	}
	if src.AllAppointments != nil {
		dst.AllAppointments = src.AllAppointments
	}
	if src.Headless != nil {
		dst.Headless = src.Headless
	}
}
