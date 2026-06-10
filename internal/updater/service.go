package updater

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"insurance-benefit-agent-go/internal/config"
	"insurance-benefit-agent-go/internal/version"
)

type Manifest struct {
	Version string `json:"version"`
	Commit  string `json:"commit,omitempty"`
	BuiltAt string `json:"builtAt,omitempty"`
	Channel string `json:"channel,omitempty"`
	Asset   string `json:"asset"`
	SHA256  string `json:"sha256"`
	// TODO(patcon): add AssetURL string `json:"assetURL,omitempty"` once patcon serves the download URL in the manifest
}

type CheckResult struct {
	Enabled         bool         `json:"enabled"`
	UpdateAvailable bool         `json:"updateAvailable"`
	Current         version.Info `json:"current"`
	Manifest        *Manifest    `json:"manifest,omitempty"`
	ManifestPath    string       `json:"manifestPath,omitempty"`
	AssetPath       string       `json:"assetPath,omitempty"`
	Reason          string       `json:"reason,omitempty"`
}

type ApplyResult struct {
	Started     bool        `json:"started"`
	Check       CheckResult `json:"check"`
	UpdaterPath string      `json:"updaterPath,omitempty"`
	TargetPath  string      `json:"targetPath,omitempty"`
	BackupPath  string      `json:"backupPath,omitempty"`
	Message     string      `json:"message,omitempty"`
}

type Service struct {
	cfg        config.UpdateConfig
	patconURL  string // base URL from bootstrapUrl.patcon.url, used when source == "patcon"
	token      string // token from bootstrapUrl.patcon.token, used when source == "patcon"
	configPath string
	args       []string
	exePath    string
}

func New(cfg config.UpdateConfig, patconURL, token, configPath string, args []string) (*Service, error) {
	exePath, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("resolve executable path: %w", err)
	}
	return &Service{
		cfg:        cfg,
		patconURL:  strings.TrimRight(patconURL, "/"),
		token:      token,
		configPath: configPath,
		args:       append([]string(nil), args...),
		exePath:    exePath,
	}, nil
}

func (s *Service) Check() CheckResult {
	current := version.Get()
	result := CheckResult{
		Enabled: s.cfg.Enabled,
		Current: current,
	}
	if !s.cfg.Enabled {
		result.Reason = "updates disabled"
		return result
	}

	switch strings.ToLower(strings.TrimSpace(s.cfg.Source)) {
	case "local":
		manifestPath := filepath.Join(s.installDir(), s.cfg.LocalDir, "manifest.json")
		result.ManifestPath = manifestPath
		manifest, err := readManifest(manifestPath)
		if err != nil {
			result.Reason = err.Error()
			return result
		}
		result.Manifest = manifest

		if manifest.Asset == "" {
			result.Reason = "manifest asset is empty"
			return result
		}
		assetPath := filepath.Join(s.installDir(), s.cfg.LocalDir, manifest.Asset)
		result.AssetPath = assetPath

	case "patcon":
		if s.patconURL == "" {
			result.Reason = "bootstrapUrl.patcon.url is required for source \"patcon\""
			return result
		}
		manifestURL := s.patconURL + "/updates/manifest.json"
		manifest, err := fetchManifest(manifestURL, s.token)
		if err != nil {
			result.Reason = err.Error()
			return result
		}
		result.Manifest = manifest
		result.ManifestPath = manifestURL
		// AssetPath intentionally empty here — binary is downloaded in Apply(), not Check()
	default:
		result.Reason = fmt.Sprintf("unsupported update source %q", s.cfg.Source)
		return result
	}

	if result.Manifest == nil {
		result.Reason = "manifest not loaded"
		return result
	}
	manifest := result.Manifest

	if !channelAllowed(s.cfg.Channel, manifest.Channel) {
		result.Reason = fmt.Sprintf("manifest channel %q not allowed by configured channel %q", manifest.Channel, s.cfg.Channel)
		return result
	}
	cmp, err := compareVersions(manifest.Version, current.Version)
	if err != nil {
		result.Reason = err.Error()
		return result
	}
	if cmp <= 0 {
		result.Reason = "current version is up to date"
		return result
	}
	// For local source, verify the asset is present and matches the manifest SHA256.
	// For patcon source, AssetPath is empty — the binary is downloaded in Apply(),
	// so verification happens there after the download.
	if result.AssetPath != "" {
		if err := verifySHA256(result.AssetPath, manifest.SHA256); err != nil {
			result.Reason = err.Error()
			return result
		}
	}

	result.UpdateAvailable = true
	result.Reason = "update available"
	return result
}

func (s *Service) Apply() (ApplyResult, error) {
	check := s.Check()
	result := ApplyResult{Check: check}
	if !check.UpdateAvailable {
		result.Message = check.Reason
		return result, nil
	}

	updaterPath := filepath.Join(s.installDir(), "agent-updater.exe")
	if _, err := os.Stat(updaterPath); err != nil {
		return result, fmt.Errorf("agent-updater.exe unavailable: %w", err)
	}

	if strings.ToLower(strings.TrimSpace(s.cfg.Source)) == "patcon" {
		assetName := check.Manifest.Asset
		downloadURL := s.patconURL + "/updates/" + assetName
		tmpPath := filepath.Join(s.installDir(), "updates", assetName+".tmp")
		if err := downloadFile(downloadURL, tmpPath, s.token); err != nil {
			return result, fmt.Errorf("download update: %w", err)
		}
		if err := verifySHA256(tmpPath, check.Manifest.SHA256); err != nil {
			_ = os.Remove(tmpPath)
			return result, fmt.Errorf("update asset verification failed: %w", err)
		}
		check.AssetPath = tmpPath
	}

	backupPath := s.exePath + ".bak"
	argsJSON, err := json.Marshal(s.restartArgs())
	if err != nil {
		return result, fmt.Errorf("marshal restart args: %w", err)
	}
	argsEncoded := base64.StdEncoding.EncodeToString(argsJSON)

	cmd := exec.Command(updaterPath,
		"--pid", strconv.Itoa(os.Getpid()),
		"--target", s.exePath,
		"--source", check.AssetPath,
		"--backup", backupPath,
		"--restart", s.exePath,
		"--args", argsEncoded,
	)
	cmd.Dir = s.installDir()
	if err := cmd.Start(); err != nil {
		return result, fmt.Errorf("start updater: %w", err)
	}

	result.Started = true
	result.UpdaterPath = updaterPath
	result.TargetPath = s.exePath
	result.BackupPath = backupPath
	result.Message = "updater started; agent will exit"
	return result, nil
}

func (s *Service) installDir() string {
	return filepath.Dir(s.exePath)
}

func (s *Service) restartArgs() []string {
	if len(s.args) > 0 {
		return append([]string(nil), s.args...)
	}
	if s.configPath != "" {
		return []string{"--config", s.configPath}
	}
	return nil
}

func readManifest(path string) (*Manifest, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read manifest: %w", err)
	}
	var manifest Manifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		return nil, fmt.Errorf("parse manifest: %w", err)
	}
	return &manifest, nil
}

var httpClient = &http.Client{Timeout: 60 * time.Second}

func fetchManifest(url, token string) (*Manifest, error) {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("fetch manifest: %w", err)
	}
	if token != "" {
		req.Header.Set("Authorization", token)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch manifest: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch manifest: unexpected status %s", resp.Status)
	}
	var manifest Manifest
	if err := json.NewDecoder(resp.Body).Decode(&manifest); err != nil {
		return nil, fmt.Errorf("parse manifest: %w", err)
	}
	return &manifest, nil
}

func downloadFile(url, destPath, token string) error {
	if err := os.MkdirAll(filepath.Dir(destPath), 0o755); err != nil {
		return fmt.Errorf("create download dir: %w", err)
	}
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("download %s: %w", url, err)
	}
	if token != "" {
		req.Header.Set("Authorization", token)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("download %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download %s: unexpected status %s", url, resp.Status)
	}
	f, err := os.Create(destPath)
	if err != nil {
		return fmt.Errorf("create file: %w", err)
	}
	defer f.Close()
	if _, err := io.Copy(f, resp.Body); err != nil {
		return fmt.Errorf("write file: %w", err)
	}
	return nil
}

func verifySHA256(path, expected string) error {
	expected = normalizeSHA256(expected)
	if expected == "" {
		return fmt.Errorf("manifest sha256 is empty")
	}
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open update asset: %w", err)
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return fmt.Errorf("hash update asset: %w", err)
	}
	actual := hex.EncodeToString(h.Sum(nil))
	if !strings.EqualFold(actual, expected) {
		return fmt.Errorf("sha256 mismatch: expected %s got %s", expected, actual)
	}
	return nil
}

func normalizeSHA256(value string) string {
	value = strings.TrimSpace(value)
	if strings.Contains(value, " ") {
		value = strings.Fields(value)[0]
	}
	value = strings.TrimPrefix(strings.ToLower(value), "sha256:")
	return value
}

func channelAllowed(configured, candidate string) bool {
	configured = strings.ToLower(strings.TrimSpace(configured))
	candidate = strings.ToLower(strings.TrimSpace(candidate))
	if configured == "" {
		configured = "stable"
	}
	if candidate == "" {
		candidate = "stable"
	}
	switch configured {
	case "beta":
		return candidate == "beta" || candidate == "rc" || candidate == "stable"
	case "rc":
		return candidate == "rc" || candidate == "stable"
	case "stable":
		return candidate == "stable"
	default:
		return candidate == configured
	}
}

func compareVersions(left, right string) (int, error) {
	l, err := parseVersion(left)
	if err != nil {
		return 0, fmt.Errorf("parse candidate version: %w", err)
	}
	r, err := parseVersion(right)
	if err != nil {
		return 0, fmt.Errorf("parse current version: %w", err)
	}
	for i := 0; i < 3; i++ {
		if l.core[i] > r.core[i] {
			return 1, nil
		}
		if l.core[i] < r.core[i] {
			return -1, nil
		}
	}
	return comparePrerelease(l.pre, r.pre), nil
}

type parsedVersion struct {
	core [3]int
	pre  string
}

func parseVersion(value string) (parsedVersion, error) {
	value = strings.TrimPrefix(strings.TrimSpace(value), "v")
	if value == "" || value == "dev" {
		return parsedVersion{}, nil
	}
	corePart, pre, _ := strings.Cut(value, "-")
	parts := strings.Split(corePart, ".")
	if len(parts) != 3 {
		return parsedVersion{}, fmt.Errorf("version %q must be major.minor.patch", value)
	}
	var out parsedVersion
	out.pre = pre
	for i, part := range parts {
		n, err := strconv.Atoi(part)
		if err != nil {
			return parsedVersion{}, fmt.Errorf("invalid version number %q", part)
		}
		out.core[i] = n
	}
	return out, nil
}

func comparePrerelease(left, right string) int {
	if left == right {
		return 0
	}
	if left == "" {
		return 1
	}
	if right == "" {
		return -1
	}
	leftRank, leftNum := prereleaseRank(left)
	rightRank, rightNum := prereleaseRank(right)
	if leftRank != rightRank {
		if leftRank > rightRank {
			return 1
		}
		return -1
	}
	if leftNum > rightNum {
		return 1
	}
	if leftNum < rightNum {
		return -1
	}
	return strings.Compare(left, right)
}

func prereleaseRank(value string) (int, int) {
	value = strings.ToLower(value)
	switch {
	case strings.HasPrefix(value, "rc."):
		return 3, parsePrereleaseNumber(value)
	case strings.HasPrefix(value, "beta."):
		return 2, parsePrereleaseNumber(value)
	case strings.HasPrefix(value, "alpha."):
		return 1, parsePrereleaseNumber(value)
	default:
		return 0, parsePrereleaseNumber(value)
	}
}

func parsePrereleaseNumber(value string) int {
	parts := strings.Split(value, ".")
	if len(parts) < 2 {
		return 0
	}
	n, _ := strconv.Atoi(parts[len(parts)-1])
	return n
}
