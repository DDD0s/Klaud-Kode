package config

import "testing"

func TestLoadHarnessDefaultsOnLoopback(t *testing.T) {
	cfg, err := Load(map[string]string{"KOMPUTER_API_HOST": "127.0.0.1"})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MaxClientToolRuns != 16 {
		t.Fatalf("MaxClientToolRuns = %d, want 16", cfg.MaxClientToolRuns)
	}
	if cfg.ClientToolTimeout <= 0 {
		t.Fatalf("ClientToolTimeout = %v", cfg.ClientToolTimeout)
	}
}
