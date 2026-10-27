package api

import (
	"sync"
	"testing"
)

// Health checks and turns probe the guard concurrently; -race must stay
// quiet, which is why every path goes through one mutex.
func TestTunnelGuardConcurrentAccess(t *testing.T) {
	server, _ := newBareServer(t)
	server.cfg.BodyConnect = true
	server.cfg.BodyToken = "0123456789abcdef0123456789abcdef"
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				_ = server.tunnel.ensure()
				_, _ = server.tunnel.status()
			}
		}()
	}
	wg.Wait()
}
