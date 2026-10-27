package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/DDD0s/Kloud-Kode/Komputer/internal/config"
	"github.com/coder/websocket"
)

// dialStubBody connects a stub Body that echoes each request id back inside
// the result text, so a test can tell whose reply is whose.
func dialStubBody(t *testing.T, ts *httptest.Server, token string) *websocket.Conn {
	t.Helper()
	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/body/connect"
	header := http.Header{}
	header.Set("Authorization", "Bearer "+token)
	conn, _, err := websocket.Dial(context.Background(), wsURL, &websocket.DialOptions{HTTPHeader: header})
	if err != nil {
		t.Fatalf("dial stub body: %v", err)
	}
	go func() {
		for {
			_, data, err := conn.Read(context.Background())
			if err != nil {
				return
			}
			var env struct {
				ID json.RawMessage `json:"id"`
			}
			_ = json.Unmarshal(data, &env)
			if len(env.ID) == 0 {
				continue
			}
			reply := fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"result":{"echo":%s}}`, env.ID, env.ID)
			_ = conn.Write(context.Background(), websocket.MessageText, []byte(reply))
		}
	}()
	return conn
}

func waitConnected(t *testing.T, server *Server) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); !server.bodyLink.Connected(); {
		if time.Now().After(deadline) {
			t.Fatal("link never registered")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func postMCP(t *testing.T, ts *httptest.Server, token, body string) (int, []byte) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/body/mcp", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data := make([]byte, 0, 512)
	buf := make([]byte, 512)
	for {
		n, err := resp.Body.Read(buf)
		data = append(data, buf[:n]...)
		if err != nil {
			break
		}
	}
	return resp.StatusCode, data
}

// Two MCP sessions may legitimately use the same JSON-RPC id at once (both
// CLIs start numbering at 1). The proxy must not confuse or reject them.
func TestBodyLinkConcurrentSameClientID(t *testing.T) {
	server, _ := newBareServer(t)
	server.cfg.BodyToken = "tok-0123456789abcdef0123456789"
	server.cfg.BodyConnect = true
	ts := httptest.NewServer(server.Handler())
	defer ts.Close()
	conn := dialStubBody(t, ts, server.cfg.BodyToken)
	defer conn.Close(websocket.StatusNormalClosure, "done")
	waitConnected(t, server)

	var wg sync.WaitGroup
	errs := make(chan error, 2)
	echoes := make(chan string, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(tag int) {
			defer wg.Done()
			body := fmt.Sprintf(`{"jsonrpc":"2.0","id":"same","method":"tools/call","params":{"n":%d}}`, tag)
			status, data := postMCP(t, ts, server.cfg.BodyToken, body)
			if status != 200 {
				errs <- fmt.Errorf("caller %d: status %d: %s", tag, status, data)
				return
			}
			// The caller's id is restored; the echo proves which internal id
			// the Body actually saw, and the two must differ.
			if !strings.Contains(string(data), `"id":"same"`) {
				errs <- fmt.Errorf("caller %d: id not restored: %s", tag, data)
				return
			}
			var decoded struct {
				Result struct {
					Echo string `json:"echo"`
				} `json:"result"`
			}
			if err := json.Unmarshal(data, &decoded); err != nil {
				errs <- err
				return
			}
			echoes <- decoded.Result.Echo
		}(i)
	}
	wg.Wait()
	close(errs)
	close(echoes)
	for err := range errs {
		t.Error(err)
	}
	seen := map[string]bool{}
	for e := range echoes {
		if seen[e] {
			t.Fatalf("internal id %q used twice", e)
		}
		seen[e] = true
	}
	if len(seen) != 2 {
		t.Fatalf("want 2 distinct internal ids, got %v", seen)
	}
}

// A JSON-RPC batch must be forwarded element by element and answered as one
// array, in order, with the caller's own ids - not dropped as a notification.
func TestBodyLinkBatch(t *testing.T) {
	server, _ := newBareServer(t)
	server.cfg.BodyToken = "tok-0123456789abcdef0123456789"
	server.cfg.BodyConnect = true
	ts := httptest.NewServer(server.Handler())
	defer ts.Close()
	conn := dialStubBody(t, ts, server.cfg.BodyToken)
	defer conn.Close(websocket.StatusNormalClosure, "done")
	waitConnected(t, server)

	body := `[{"jsonrpc":"2.0","id":"a","method":"tools/call","params":{}},{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{}},{"jsonrpc":"2.0","method":"notifications/initialized"}]`
	status, data := postMCP(t, ts, server.cfg.BodyToken, body)
	if status != 200 {
		t.Fatalf("batch status %d: %s", status, data)
	}
	var replies []map[string]any
	if err := json.Unmarshal(data, &replies); err != nil {
		t.Fatalf("batch reply not an array: %v (%s)", err, data)
	}
	if len(replies) != 2 {
		t.Fatalf("want 2 replies (notification omitted), got %d: %s", len(replies), data)
	}
	if replies[0]["id"] != "a" || replies[1]["id"] != float64(2) {
		t.Fatalf("ids not restored in order: %s", data)
	}
}

// With TLS listeners the loopback proxy URL handed to the CLI must be https.
func TestProxyBaseURLSchemeAndHost(t *testing.T) {
	cfg := config.Config{Port: 18888, TLSCertFile: "/tmp/cert.pem", TLSKeyFile: "/tmp/key.pem", Listen: []config.ListenAddress{{Host: "0.0.0.0", Port: 8443}}}
	if got, want := proxyBaseURL(&cfg), "https://127.0.0.1:8443"; got != want {
		t.Fatalf("TLS proxyBaseURL = %q, want %q", got, want)
	}
}
