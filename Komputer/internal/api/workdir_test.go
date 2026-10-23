package api

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/DDD0s/Kloud-Kode/Komputer/internal/backend"
	"github.com/DDD0s/Kloud-Kode/Komputer/internal/config"
)

// A denied work directory must not stop the service: it falls back beside the
// service and the backend follows.
func TestWorkDirFallsBackWhenDenied(t *testing.T) {
	// Keep the test fast: the real delays exist for antivirus, not for us.
	attempts, delay := workDirAttempts, workDirDelay
	workDirAttempts, workDirDelay = 2, time.Millisecond
	t.Cleanup(func() { workDirAttempts, workDirDelay = attempts, delay })
	server, _ := newBareServer(t)
	// Point the backend at a path that cannot exist on any platform.
	missing := filepath.Join("Z:", "no-such-root", "deep", "cwd")
	if _, err := os.Stat(missing); err == nil {
		t.Skip("Z: exists on this host")
	}
	claude := backend.NewClaude(config.Claude{Cwd: missing, ModelID: "komputer-claude"})
	if err := server.ensureWorkDir(claude); err != nil {
		t.Fatalf("fallback failed: %v", err)
	}
	if claude.Cwd() == missing {
		t.Fatal("cwd was not redirected")
	}
	if filepath.Dir(claude.Cwd()) != server.cfg.BaseDir {
		t.Fatalf("fallback not beside the service: %q", claude.Cwd())
	}
	info, err := os.Stat(claude.Cwd())
	if err != nil || !info.IsDir() {
		t.Fatalf("fallback directory missing: %v", err)
	}
}
