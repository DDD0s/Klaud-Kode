package body

import (
	"os"
	"path/filepath"
	"testing"
)

// Regression: brainUrl in the config file (and KLOUD_KODE_BRAIN_URL) must
// actually reach Config. It was parsed into rawConfig and dropped, so only
// --to on the command line ever worked.
func TestLoadConfigBrainURL(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	body := `{"token":"0123456789abcdef0123456789abcdef","roots":{"home":"~"},"defaultRoot":"home","brainUrl":"wss://brain.example"}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KLOUD_KODE_BODY_CONFIG", path)
	t.Setenv("KLOUD_KODE_BRAIN_URL", "")
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.BrainURL != "wss://brain.example" {
		t.Fatalf("BrainURL from file = %q", cfg.BrainURL)
	}

	t.Setenv("KLOUD_KODE_BRAIN_URL", "https://other.example")
	cfg, err = LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.BrainURL != "https://other.example" {
		t.Fatalf("BrainURL from env = %q", cfg.BrainURL)
	}
}
