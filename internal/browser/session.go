package browser

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/launcher"
	"github.com/go-rod/rod/lib/proto"
)

type Session struct {
	Browser  *rod.Browser
	Page     *rod.Page
	launcher *launcher.Launcher // nil for UserMode; used to kill the process tree on Close
	pid      int
	tempDir  string
}

type LaunchOptions struct {
	StorageStatePath string
	Headless         bool
	// UserMode connects to the user's real installed Chrome instead of the
	// bundled Chromium. Use for sites that fingerprint CDP-controlled browsers.
	UserMode bool
}

func Launch(options ...LaunchOptions) (*Session, error) {
	var opts LaunchOptions
	if len(options) > 0 {
		opts = options[0]
	}

	if !opts.UserMode {
		killStaleAgentChromeProcesses()
	}

	const maxAttempts = 3
	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		session, err := launchAttempt(opts, attempt, maxAttempts)
		if err == nil {
			return session, nil
		}
		lastErr = err
		if attempt < maxAttempts {
			log.Printf("[browser] launch attempt %d/%d failed: %v; retrying in 2s", attempt, maxAttempts, err)
			time.Sleep(2 * time.Second)
		}
	}
	if lastErr != nil {
		return nil, lastErr
	}
	return nil, fmt.Errorf("launch browser failed")
}

func launchAttempt(opts LaunchOptions, attempt int, maxAttempts int) (*Session, error) {
	var controlURL string
	var launcherRef *launcher.Launcher
	var tempDir string
	var pid int
	var err error

	if opts.UserMode {
		controlURL, err = launcher.NewUserMode().
			UserDataDir("./chrome-profile").
			Set("start-maximized").
			Delete("enable-automation").
			Set("disable-blink-features", "AutomationControlled").
			Launch()
	} else {
		tempDir, err = os.MkdirTemp("", "agent-chrome-*")
		if err != nil {
			return nil, fmt.Errorf("create chrome temp dir: %w", err)
		}
		l := ConfigureLauncher(launcher.New()).
			UserDataDir(tempDir).
			Leakless(false). // leakless.exe is flagged by Windows Defender
			Delete("enable-automation").
			Set("disable-blink-features", "AutomationControlled")
		if opts.Headless {
			l = l.HeadlessNew(true)
		} else {
			l = l.Headless(false).
				Delete("headless").
				Delete("hide-scrollbars").
				Delete("mute-audio")
			l = l.Set("start-maximized")
		}
		controlURL, err = l.Launch()
		if err == nil {
			launcherRef = l
			pid = l.PID()
		}
	}
	if err != nil {
		cleanupLaunchedBrowser(nil, launcherRef, pid, tempDir)
		return nil, fmt.Errorf("launch browser: %w", err)
	}

	browser := rod.New().ControlURL(controlURL)
	if err := connectBrowserWithTimeout(browser, 20*time.Second); err != nil {
		cleanupLaunchedBrowser(browser, launcherRef, pid, tempDir)
		return nil, fmt.Errorf("connect to browser: %w", err)
	}

	if opts.StorageStatePath != "" {
		if err := loadCookiesWithTimeout(browser, opts.StorageStatePath, 5*time.Second); err != nil {
			if !os.IsNotExist(err) {
				log.Printf("[browser] storage restore failed: %v", err)
			}
		}
	}

	page, err := browser.Page(proto.TargetCreateTarget{URL: "about:blank"})
	if err != nil {
		cleanupLaunchedBrowser(browser, launcherRef, pid, tempDir)
		return nil, fmt.Errorf("create page: %w", err)
	}
	if err := applyDesktopViewport(page); err != nil {
		cleanupLaunchedBrowser(browser, launcherRef, pid, tempDir)
		return nil, fmt.Errorf("apply browser viewport: %w", err)
	}

	return &Session{
		Browser:  browser,
		Page:     page,
		launcher: launcherRef,
		pid:      pid,
		tempDir:  tempDir,
	}, nil
}

func cleanupLaunchedBrowser(browser *rod.Browser, launcherRef *launcher.Launcher, pid int, tempDir string) {
	forceKillProcessTree(pid)
	if launcherRef != nil {
		launcherRef.Kill()
	}
	if browser != nil {
		done := make(chan struct{}, 1)
		go func() {
			_ = browser.Close()
			done <- struct{}{}
		}()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
		}
	}
	if tempDir != "" {
		_ = os.RemoveAll(tempDir)
	}
}

func connectBrowserWithTimeout(browser *rod.Browser, timeout time.Duration) error {
	done := make(chan error, 1)
	go func() {
		done <- browser.Connect()
	}()
	select {
	case err := <-done:
		return err
	case <-time.After(timeout):
		return fmt.Errorf("timed out after %s", timeout)
	}
}

func loadCookiesWithTimeout(browser *rod.Browser, path string, timeout time.Duration) error {
	done := make(chan error, 1)
	go func() {
		done <- loadCookies(browser, path)
	}()
	select {
	case err := <-done:
		return err
	case <-time.After(timeout):
		return fmt.Errorf("timed out after %s", timeout)
	}
}

func applyDesktopViewport(page *rod.Page) error {
	if page == nil {
		return fmt.Errorf("page is nil")
	}
	return proto.EmulationSetDeviceMetricsOverride{
		Width:             1600,
		Height:            1200,
		DeviceScaleFactor: 1,
		Mobile:            false,
	}.Call(page)
}

func (s *Session) SaveStorageState(path string) error {
	if s == nil || s.Browser == nil {
		return fmt.Errorf("browser is not initialized")
	}
	if path == "" {
		return fmt.Errorf("storage state path is required")
	}

	if dir := filepath.Dir(path); dir != "." && dir != "" {
		if err := os.MkdirAll(dir, 0700); err != nil {
			return fmt.Errorf("create storage state directory: %w", err)
		}
	}

	res, err := proto.StorageGetCookies{}.Call(s.Browser)
	if err != nil {
		return fmt.Errorf("get browser cookies: %w", err)
	}

	data, err := json.MarshalIndent(res.Cookies, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal cookies: %w", err)
	}

	if err := os.WriteFile(path, data, 0600); err != nil {
		return fmt.Errorf("save browser storage state: %w", err)
	}

	return nil
}

func loadCookies(browser *rod.Browser, path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}

	var cookies []*proto.NetworkCookie
	if err := json.Unmarshal(data, &cookies); err != nil {
		// File exists but isn't in our format (e.g. left over from Playwright).
		// Treat it as missing so we do a fresh login rather than hard-failing.
		return fmt.Errorf("%w: %v", os.ErrNotExist, err)
	}

	params := make([]*proto.NetworkCookieParam, len(cookies))
	for i, c := range cookies {
		params[i] = &proto.NetworkCookieParam{
			Name:     c.Name,
			Value:    c.Value,
			Domain:   c.Domain,
			Path:     c.Path,
			Secure:   c.Secure,
			HTTPOnly: c.HTTPOnly,
			SameSite: c.SameSite,
		}
	}

	return proto.StorageSetCookies{Cookies: params}.Call(browser)
}

func (s *Session) Close() error {
	if s == nil || s.Browser == nil {
		return nil
	}
	page := s.Page
	browser := s.Browser
	launcherRef := s.launcher
	pid := s.pid
	tempDir := s.tempDir
	s.Page = nil
	s.Browser = nil
	s.launcher = nil
	s.pid = 0
	s.tempDir = ""

	if page != nil {
		_ = page.Close()
	}

	// Kill the entire Chrome process tree while the launcher PID still exists.
	// If Browser.Close() wins the race first on Windows, child chrome.exe
	// processes can be re-parented and survive into the next payer run.
	forceKillProcessTree(pid)
	if launcherRef != nil {
		launcherRef.Kill()
	}

	var err error
	done := make(chan error, 1)
	go func() {
		done <- browser.Close()
	}()
	select {
	case err = <-done:
	case <-time.After(5 * time.Second):
		err = fmt.Errorf("browser close timed out")
	}

	if tempDir != "" {
		_ = os.RemoveAll(tempDir)
	}
	if err != nil && strings.Contains(err.Error(), "use of closed network connection") {
		return nil
	}
	return err
}

// ForceKillProcessTree sends taskkill /F /T to the given PID on Windows,
// terminating the process and all its children. No-op on other platforms.
func ForceKillProcessTree(pid int) { forceKillProcessTree(pid) }

func forceKillProcessTree(pid int) {
	if pid <= 0 || runtime.GOOS != "windows" {
		return
	}
	cmd := exec.Command("taskkill", "/PID", fmt.Sprintf("%d", pid), "/T", "/F")
	if out, err := cmd.CombinedOutput(); err != nil {
		text := strings.TrimSpace(string(out))
		if isTaskkillProcessNotFound(text) {
			return
		}
		if text != "" {
			log.Printf("[browser] taskkill pid=%d failed: %v: %s", pid, err, text)
		}
	}
}

func isTaskkillProcessNotFound(output string) bool {
	lower := strings.ToLower(output)
	if strings.Contains(lower, "success:") && strings.Contains(lower, "no running instance") {
		return true
	}
	return strings.Contains(lower, "not found") ||
		strings.Contains(lower, "not running") ||
		strings.Contains(lower, "no running instance")
}

func killStaleAgentChromeProcesses() {
	if runtime.GOOS != "windows" {
		return
	}
	matches, err := filepath.Glob(filepath.Join(os.TempDir(), "agent-chrome-*"))
	if err != nil {
		log.Printf("[browser] stale chrome cleanup glob failed: %v", err)
		return
	}
	for _, dir := range matches {
		port, err := readDevToolsPort(dir)
		if err != nil {
			_ = os.RemoveAll(dir)
			continue
		}
		pid, err := pidListeningOnPort(port)
		if err != nil {
			log.Printf("[browser] stale chrome cleanup could not resolve port=%s dir=%s: %v", port, dir, err)
			_ = os.RemoveAll(dir)
			continue
		}
		if pid > 0 {
			forceKillProcessTree(pid)
			log.Printf("[browser] cleaned stale agent chrome pid=%d port=%s dir=%s", pid, port, dir)
		}
		_ = os.RemoveAll(dir)
	}
}

func readDevToolsPort(profileDir string) (string, error) {
	raw, err := os.ReadFile(filepath.Join(profileDir, "DevToolsActivePort"))
	if err != nil {
		return "", err
	}
	lines := strings.Fields(string(raw))
	if len(lines) == 0 || strings.TrimSpace(lines[0]) == "" {
		return "", fmt.Errorf("DevToolsActivePort has no port")
	}
	return strings.TrimSpace(lines[0]), nil
}

func pidListeningOnPort(port string) (int, error) {
	out, err := exec.Command("netstat", "-ano", "-p", "tcp").CombinedOutput()
	if err != nil {
		return 0, fmt.Errorf("netstat: %w", err)
	}
	needle := ":" + port
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 5 || !strings.EqualFold(fields[0], "TCP") {
			continue
		}
		if !strings.Contains(fields[1], needle) || !strings.EqualFold(fields[3], "LISTENING") {
			continue
		}
		var pid int
		if _, scanErr := fmt.Sscanf(fields[4], "%d", &pid); scanErr != nil {
			return 0, fmt.Errorf("parse netstat pid from %q: %w", line, scanErr)
		}
		return pid, nil
	}
	return 0, nil
}
