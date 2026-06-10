package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadAppliesTestingFromAgentLocalJSON(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "agent.config.json")
	localPath := filepath.Join(dir, "agent.local.json")
	if err := os.WriteFile(configPath, []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(localPath, []byte(`{
		"testing": {
			"writePdf": true,
			"localPdfOnly": true,
			"skipApptField": true,
			"skipTracking": true
		}
	}`), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Testing.WritePDF == nil || !*cfg.Testing.WritePDF {
		t.Fatal("expected local testing.writePdf to apply")
	}
	if !cfg.Testing.ShouldUsePDFLocalOnly() {
		t.Fatal("expected local testing.localPdfOnly to apply")
	}
	if cfg.Testing.ShouldUpdateApptField() {
		t.Fatal("expected local testing.skipApptField to apply")
	}
	if !cfg.Testing.ShouldSkipTracking() {
		t.Fatal("expected local testing.skipTracking to apply")
	}
}
