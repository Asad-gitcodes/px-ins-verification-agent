package browser

import (
	"log"
	"os"
	"path/filepath"
	"runtime"

	"github.com/go-rod/rod/lib/launcher"
)

const browserBinEnv = "AGENT_BROWSER_BIN"

// ConfigureLauncher pins rod to a local browser binary when one is available.
// That prevents rod from downloading Chromium on locked-down client networks.
func ConfigureLauncher(l *launcher.Launcher) *launcher.Launcher {
	if l == nil {
		return nil
	}
	if binPath, _, ok := ResolveBrowserBinPath(); ok {
		return l.Bin(binPath)
	}
	log.Printf("[browser] no browser binary found; rod may download Chromium")
	return l
}

func ResolveBrowserBinPath() (path string, source string, ok bool) {
	if path := os.Getenv(browserBinEnv); path != "" {
		if fileExists(path) {
			return path, browserBinEnv, true
		}
		log.Printf("[browser] %s is set but not found path=%s", browserBinEnv, path)
	}

	if path, ok := packagedBrowserBinPath(); ok {
		return path, "packaged", true
	}

	if path, ok := launcher.LookPath(); ok && fileExists(path) {
		return path, "installed", true
	}

	return "", "", false
}

func packagedBrowserBinPath() (string, bool) {
	exePath, err := os.Executable()
	if err != nil {
		log.Printf("[browser] resolve executable path failed: %v", err)
		return "", false
	}
	baseDir := filepath.Dir(exePath)

	for _, rel := range packagedBrowserCandidates() {
		path := filepath.Join(baseDir, rel)
		if fileExists(path) {
			return path, true
		}
	}
	return "", false
}

func packagedBrowserCandidates() []string {
	exeName := map[string]string{
		"darwin":  "Chromium.app/Contents/MacOS/Chromium",
		"linux":   "chrome",
		"windows": "chrome.exe",
	}[runtime.GOOS]
	if exeName == "" {
		exeName = "chrome"
	}

	return []string{
		filepath.Join("browser", exeName),
		filepath.Join("chromium", exeName),
		filepath.Join("chrome", exeName),
	}
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}
