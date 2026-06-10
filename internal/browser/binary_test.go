package browser

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResolveBrowserBinPathUsesEnvOverride(t *testing.T) {
	dir := t.TempDir()
	binPath := filepath.Join(dir, "chrome.exe")
	if err := os.WriteFile(binPath, []byte("test"), 0o644); err != nil {
		t.Fatalf("write test browser binary: %v", err)
	}
	t.Setenv(browserBinEnv, binPath)

	got, source, ok := ResolveBrowserBinPath()
	if !ok {
		t.Fatalf("ResolveBrowserBinPath returned ok=false")
	}
	if got != binPath {
		t.Fatalf("ResolveBrowserBinPath path=%q, want %q", got, binPath)
	}
	if source != browserBinEnv {
		t.Fatalf("ResolveBrowserBinPath source=%q, want %q", source, browserBinEnv)
	}
}
