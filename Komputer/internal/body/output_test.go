package body

import (
	"strings"
	"sync"
	"testing"
)

// The collector is written by the command's copy goroutines, stdout and
// stderr at once, and read after Wait. It has to be race-free and to keep
// the first and last bytes.
func TestHeadAndTailConcurrentWrites(t *testing.T) {
	h := NewHeadAndTail(64)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				_, _ = h.Write([]byte("abcdefgh"))
			}
		}()
	}
	wg.Wait()
	got, dropped := h.Finish()
	if dropped == 0 {
		t.Fatalf("expected middle bytes to be dropped: %q", got)
	}
	if len(got) > 64+64 {
		t.Fatalf("collector exceeded its bound: %d bytes", len(got))
	}
	if !strings.HasPrefix(string(got), "abc") {
		t.Fatalf("head lost: %q", got)
	}
	if !strings.Contains(string(got), "dropped from the middle") {
		t.Fatalf("missing drop marker: %q", got)
	}
}

// A very fast command must not lose its output to the wait race.
func TestRunCommandCapturesFastOutput(t *testing.T) {
	cfg := Config{MaxOutputBytes: 4096, ConsoleUTF8: true}
	// "default" resolves per platform, so this runs on Windows too. The
	// comparison trims, because cmd.exe appends CRLF.
	for i := 0; i < 40; i++ {
		res, err := RunCommand(cfg, "echo FAST_OUTPUT", t.TempDir(), "default", 10000)
		if err != nil {
			t.Fatal(err)
		}
		if res.ExitCode == nil || *res.ExitCode != 0 {
			t.Fatalf("run %d failed: %+v", i, res)
		}
		if strings.TrimSpace(res.Stdout) != "FAST_OUTPUT" {
			t.Fatalf("run %d lost output: %q", i, res.Stdout)
		}
	}
}
