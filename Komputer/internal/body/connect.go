package body

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
)

// A plaintext link may only be used where the network itself is the boundary:
// loopback and private addresses. KLOUD_KODE_BRAIN_ALLOW_PLAINTEXT=1 overrides.
func plaintextAllowed(host string) bool {
	if os.Getenv("KLOUD_KODE_BRAIN_ALLOW_PLAINTEXT") == "1" {
		return true
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && (ip.IsLoopback() || ip.IsPrivate())
}

// ServeConnect keeps this Body attached to the brain over one outbound
// WebSocket. It exists for computers nobody can dial into -- a family PC
// behind home NAT, on another continent -- where the only reliable direction
// is out. The Body redials forever with backoff until ctx ends, because the
// machine is unattended: a watchdog restarting it should not be needed.
func (s *Service) ServeConnect(ctx context.Context, brainURL string) error {
	if len(s.cfg.Token) < 24 {
		return fmt.Errorf("connect mode needs a token of at least 24 characters (set token in %s or KLOUD_KODE_BODY_TOKEN)", s.cfg.ConfigPath)
	}
	target, err := connectURL(brainURL)
	if err != nil {
		return err
	}
	backoff := time.Second
	for {
		started := time.Now()
		connected := s.connectOnce(ctx, target)
		if ctx.Err() != nil {
			return nil
		}
		// Only a link that lived a while earns an immediate redial; one dropped
		// on arrival (flapping proxy, brain restarting) must back off, or an
		// unattended machine hammers the brain forever.
		if connected && time.Since(started) >= 10*time.Second {
			backoff = time.Second
		}
		wait := backoff + time.Duration(rand.Int64N(int64(backoff)/4+1))
		s.logf("brain link down; redialing in %s", wait)
		select {
		case <-time.After(wait):
		case <-ctx.Done():
			return nil
		}
		backoff = min(backoff*2, 30*time.Second)
	}
}

// connectURL accepts the API base (https://brain.example) or a full ws/wss URL
// and returns the WebSocket endpoint to dial.
func connectURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", fmt.Errorf("connect mode needs a brain URL (set brainUrl in the config, KLOUD_KODE_BRAIN_URL, or --to)")
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("brain URL: %w", err)
	}
	switch parsed.Scheme {
	case "https":
		parsed.Scheme = "wss"
	case "http":
		parsed.Scheme = "ws"
	case "ws", "wss":
	default:
		return "", fmt.Errorf("brain URL scheme %q is not http/ws", parsed.Scheme)
	}
	host := parsed.Hostname()
	if host == "" {
		return "", fmt.Errorf("brain URL %q has no host", raw)
	}
	// The token is a bearer credential; outside a private network it must not
	// cross the internet in the clear.
	if parsed.Scheme == "ws" && !plaintextAllowed(host) {
		return "", fmt.Errorf("brain URL %q is plaintext ws to a public host; use wss:// (or set KLOUD_KODE_BRAIN_ALLOW_PLAINTEXT=1 to override)", raw)
	}
	parsed.Path = strings.TrimRight(parsed.Path, "/")
	if !strings.HasSuffix(parsed.Path, "/body/connect") {
		parsed.Path += "/body/connect"
	}
	parsed.RawQuery = ""
	parsed.Fragment = ""
	return parsed.String(), nil
}

func (s *Service) connectOnce(ctx context.Context, target string) bool {
	header := http.Header{}
	header.Set("Authorization", "Bearer "+s.cfg.Token)
	conn, _, err := websocket.Dial(ctx, target, &websocket.DialOptions{HTTPHeader: header})
	if err != nil {
		s.logf("brain connect failed: %v", err)
		return false
	}
	defer conn.CloseNow()
	conn.SetReadLimit(64 << 20)
	s.logf("connected to brain at %s", target)

	// Everything spawned for this connection ends with it: handler goroutines
	// get connCtx, so a dropped link cancels work instead of leaving it
	// running unattended, and the ping loop proves the peer is still there.
	connCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		ticker := time.NewTicker(25 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-connCtx.Done():
				return
			case <-ticker.C:
				pctx, pcancel := context.WithTimeout(connCtx, 10*time.Second)
				err := conn.Ping(pctx)
				pcancel()
				if err != nil {
					_ = conn.CloseNow()
					return
				}
			}
		}
	}()

	var writeMu sync.Mutex
	sem := make(chan struct{}, 32)
	var wg sync.WaitGroup
	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			if ctx.Err() == nil {
				s.logf("brain link closed: %v", err)
			}
			cancel()
			wg.Wait()
			return true
		}
		wg.Add(1)
		go func(payload []byte) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-connCtx.Done():
				return
			}
			response, notification, ok := s.handleRPCMessage(connCtx, payload)
			if !ok || notification {
				return
			}
			out, err := json.Marshal(response)
			if err != nil {
				return
			}
			writeMu.Lock()
			_ = conn.Write(connCtx, websocket.MessageText, out)
			writeMu.Unlock()
		}(data)
	}
}
