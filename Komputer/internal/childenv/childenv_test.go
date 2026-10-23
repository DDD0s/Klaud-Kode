package childenv

import (
	"path/filepath"
	"testing"
)

func TestBuildClaudeFiltersSecretsAndAddsProxyBypass(t *testing.T) {
	source := map[string]string{
		"PATH": "/usr/bin", "HOME": "/home/test",
		"KOMPUTER_API_KEY": "no", "CUSTOM_SECRET": "no", "CLAUDE_BIN": "/x",
		"ANTHROPIC_MODEL": "m", "ANTHROPIC_FUTURE_SECRET": "no",
		"HTTP_PROXY": "http://proxy:8080", "LC_ALL": "en_US.UTF-8",
	}
	cwd := filepath.Join("work", "project")
	out := BuildClaude(source, cwd)
	if out["PATH"] != "/usr/bin" || out["ANTHROPIC_MODEL"] != "m" || out["LC_ALL"] == "" {
		t.Fatalf("kept vars missing: %v", out)
	}
	for _, k := range []string{"KOMPUTER_API_KEY", "CUSTOM_SECRET", "CLAUDE_BIN", "ANTHROPIC_FUTURE_SECRET"} {
		if _, ok := out[k]; ok {
			t.Fatalf("%s leaked into child env", k)
		}
	}
	if out["CI"] != "1" || out["GIT_CEILING_DIRECTORIES"] != filepath.Dir(cwd) {
		t.Fatalf("synthetic vars wrong: %v", out)
	}
	if out["HTTPS_PROXY"] != "http://proxy:8080" || out["NO_PROXY"] == "" {
		t.Fatalf("proxy normalization wrong: %v", out)
	}
}
