package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// LocalConfig is an optional, human-edited override file (agent.local.json)
// that lives next to the agent executable.  It is never written by the agent
// itself.  If the file is missing the agent runs with all defaults unchanged.
//
// Use this file to toggle behaviour locally without touching the server:
//   - skip a broken payer while a fix is deployed
//   - turn on verbose logging for a single office
//   - override appointment range days for testing
//   - set any future flag via the open "flags" map
type LocalConfig struct {
	// Browser controls headless / debug browser behaviour.
	Browser LocalBrowserConfig `json:"browser,omitempty"`

	// Payers allows per-office payer-level overrides.
	Payers LocalPayerConfig `json:"payers,omitempty"`

	// Overrides selectively shadow ScraperConfig values delivered by the server.
	// Only non-nil fields are applied.
	Overrides LocalOverrides `json:"overrides,omitempty"`

	// Flags is an open-ended key→value map for quick one-off toggles that do
	// not yet warrant a named field.  Values may be bool, float64, or string
	// (JSON native types).  Access them via FlagBool / FlagInt / FlagString.
	//
	// Example:
	//   "flags": { "verboseProbe": true, "probeTimeoutSec": 30 }
	Flags map[string]any `json:"flags,omitempty"`

	// Testing mirrors Config.Testing for local-only runs. This lets a single
	// agent.local.json control browser/debug overrides and test write-back flags.
	Testing TestingConfig `json:"testing,omitempty"`
}

// LocalBrowserConfig controls the embedded browser.
type LocalBrowserConfig struct {
	// Headless sets whether the browser runs without a visible window.
	// Default: true (server value or compiled default).
	Headless *bool `json:"headless,omitempty"`

	// ScreenshotOnError saves a PNG next to artifacts/ when a page action fails.
	// Default: false.
	ScreenshotOnError *bool `json:"screenshotOnError,omitempty"`

	// SlowMoMs adds a fixed delay (ms) between every browser action.
	// Useful when debugging timing issues. Default: 0.
	SlowMoMs *int `json:"slowMoMs,omitempty"`
}

// LocalPayerConfig controls which payers are processed.
type LocalPayerConfig struct {
	// SkipPayerURLs lists payer URLs to skip entirely this run.
	// Example: ["DeltaDentalIns.com"]
	SkipPayerURLs []string `json:"skipPayerUrls,omitempty"`
}

// LocalOverrides shadows individual ScraperConfig values from the server.
// A nil pointer means "use whatever the server sent".
type LocalOverrides struct {
	// ApptRangeDays overrides ScraperConfig.Office.ApptRangeDays.
	ApptRangeDays *int `json:"apptRangeDays,omitempty"`

	// ScraperConcurrency overrides ScraperConfig.ScraperConcurrency.
	ScraperConcurrency *int `json:"scraperConcurrency,omitempty"`

	// QueryLimit overrides the appointment query page size (Config.Sweep.QueryLimit).
	QueryLimit *int `json:"queryLimit,omitempty"`

	// MFAPassword overrides the MFA mailbox password from the server.
	// Useful for local testing when the server value is stale or incorrect.
	MFAPassword *string `json:"mfaPassword,omitempty"`

	// InsPDFGenerate overrides office.insPDFGenerate from the server (0 or 1).
	// Set to 1 to force PDF upload to OD even when the server sends 0.
	InsPDFGenerate *int `json:"insPDFGenerate,omitempty"`
}

// ── helpers ───────────────────────────────────────────────────────────────────

// ShouldSkipPayer returns true when payerURL is listed in Payers.SkipPayerURLs.
func (lc *LocalConfig) ShouldSkipPayer(payerURL string) bool {
	if lc == nil {
		return false
	}
	for _, skip := range lc.Payers.SkipPayerURLs {
		if strings.EqualFold(skip, payerURL) {
			return true
		}
	}
	return false
}

// IsHeadless returns the local headless setting, falling back to defaultVal.
func (lc *LocalConfig) IsHeadless(defaultVal bool) bool {
	if lc == nil || lc.Browser.Headless == nil {
		return defaultVal
	}
	return *lc.Browser.Headless
}

// ScreenshotOnError returns true if screenshot-on-error is locally enabled.
func (lc *LocalConfig) ScreenshotOnError() bool {
	if lc == nil || lc.Browser.ScreenshotOnError == nil {
		return false
	}
	return *lc.Browser.ScreenshotOnError
}

// SlowMoMs returns the browser slow-motion delay in milliseconds (0 = none).
func (lc *LocalConfig) SlowMoMs() int {
	if lc == nil || lc.Browser.SlowMoMs == nil {
		return 0
	}
	return *lc.Browser.SlowMoMs
}

// FlagBool returns the bool value of flags[key], or defaultVal if absent or
// the value is not a bool.
func (lc *LocalConfig) FlagBool(key string, defaultVal bool) bool {
	if lc == nil {
		return defaultVal
	}
	v, ok := lc.Flags[key]
	if !ok {
		return defaultVal
	}
	b, ok := v.(bool)
	if !ok {
		return defaultVal
	}
	return b
}

// FlagInt returns the int value of flags[key], or defaultVal if absent or
// the value cannot be interpreted as an integer.
func (lc *LocalConfig) FlagInt(key string, defaultVal int) int {
	if lc == nil {
		return defaultVal
	}
	v, ok := lc.Flags[key]
	if !ok {
		return defaultVal
	}
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	}
	return defaultVal
}

// FlagString returns the string value of flags[key], or defaultVal if absent.
func (lc *LocalConfig) FlagString(key string, defaultVal string) string {
	if lc == nil {
		return defaultVal
	}
	v, ok := lc.Flags[key]
	if !ok {
		return defaultVal
	}
	s, ok := v.(string)
	if !ok {
		return defaultVal
	}
	return s
}

// ── loading ───────────────────────────────────────────────────────────────────

const localConfigFileName = "agent.local.json"

// loadLocalConfig reads agent.local.json from the same directory as the main
// config file.  Returns an empty LocalConfig (all defaults) if the file does
// not exist — a missing file is not an error.
func loadLocalConfig(mainConfigPath string) (*LocalConfig, error) {
	dir := filepath.Dir(mainConfigPath)
	if dir == "" {
		dir = "."
	}
	candidates := []string{filepath.Join(dir, localConfigFileName)}
	if wd, err := os.Getwd(); err == nil {
		wdPath := filepath.Join(wd, localConfigFileName)
		if !samePath(candidates[0], wdPath) {
			candidates = append(candidates, wdPath)
		}
	}

	for _, localPath := range candidates {
		raw, err := os.ReadFile(localPath)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", localConfigFileName, err)
		}

		var lc LocalConfig
		if err := json.Unmarshal(raw, &lc); err != nil {
			return nil, fmt.Errorf("parse %s: %w", localConfigFileName, err)
		}
		return &lc, nil
	}
	return &LocalConfig{}, nil
}

func samePath(a, b string) bool {
	absA, errA := filepath.Abs(a)
	absB, errB := filepath.Abs(b)
	if errA == nil {
		a = absA
	}
	if errB == nil {
		b = absB
	}
	return strings.EqualFold(filepath.Clean(a), filepath.Clean(b))
}
