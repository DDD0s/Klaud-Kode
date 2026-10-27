package body

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
)

// ServeConnect keeps this Body attached to the brain over one outbound
// WebSocket. It exists for computers nobody can dial into -- a family PC
// behind home NAT, on another continent -- where the only reliable direction
// is out. The Body redials forever with backoff until ctx ends, because the
// machine is unattended: a watchdog restarting it should not be needed.
func (s *Service) ServeConnect(ctx context.Context, brainURL string) error {
	if len(s.cfg.Token) < 24 {
		return fmt.Errorf("connect mode needs a token of at least 24 characters (set `token` in %s or KLOUD_KODE_BODY_TOKEN)", s.cfg.ConfigPath)
	}
	target, err := connectURL(brainURL)
	if err != nil {
		return err
	}
	backoff := time.Second
	for {
		connected := s.connectOnce(ctx, target)
		if ctx.Err() != nil {
			return nil
		}
		if connected {
			backoff = time.Second
		}
		s.logf("brain link down; redialing in %s", backoff)
		select {
		case <-time.After(backoff):
		case <-ctx.Done():
			return nil
		}
		if backoff < 30*time.Second {
			backoff *= 2
		}
	}
}

// connectURL accepts the API base (https://brain.example) or a full ws/wss URL
// and returns the WebSocket endpoint to dial.
func connectURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", fmt.Errorf("connect mode needs a brain URL (set `brainUrl` in the config, KLOUD_KODE_BRAIN_URL, or --to)")
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
	if !strings.HasSuffix(parsed.Path, "/body/connect") {
		parsed.Path = strings.TrimRight(parsed.Path, "/") + "/body/connect"
	}
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
	defer conn.Close(websocket.StatusNormalClosure, "bye")
	conn.SetReadLimit(64 << 20)
	s.logf("connected to brain at %s", target)

	var writeMu sync.Mutex
	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			if ctx.Err() == nil {
				s.logf("brain link closed: %v", err)
			}
			return true
		}
		go func(payload []byte) {
			response, notification, ok := s.handleRPCMessage(ctx, payload)
			if !ok || notification {
				return
			}
			out, err := json.Marshal(response)
			if err != nil {
				return
			}
			writeMu.Lock()
			_ = conn.Write(ctx, websocket.MessageText, out)
			writeMu.Unlock()
		}(data)
	}
}
