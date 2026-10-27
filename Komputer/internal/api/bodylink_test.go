package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// A Body behind NAT dials in; the CLI's MCP calls must reach it through the
// loopback proxy, and an unauthenticated peer must not.
func TestBodyLinkRoundTrip(t *testing.T) {
	server, _ := newBareServer(t)
	server.cfg.BodyToken = "0123456789abcdef0123456789abcdef"
	server.cfg.BodyConnect = true
	ts := httptest.NewServer(server.Handler())
	defer ts.Close()
	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/body/connect"

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// A wrong token must not get a link.
	badHeader := http.Header{}
	badHeader.Set("Authorization", "Bearer wrong")
	if _, _, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{HTTPHeader: badHeader}); err == nil {
		t.Fatal("dial with a wrong token succeeded")
	}

	header := http.Header{}
	header.Set("Authorization", "Bearer "+server.cfg.BodyToken)
	conn, _, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{HTTPHeader: header})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close(websocket.StatusNormalClosure, "test done")

	go func() {
		for {
			_, data, err := conn.Read(ctx)
			if err != nil {
				return
			}
			var env struct {
				ID json.RawMessage `json:"id"`
			}
			_ = json.Unmarshal(data, &env)
			reply, _ := json.Marshal(map[string]any{
				"jsonrpc": "2.0", "id": env.ID,
				"result": map[string]any{"content": []any{map[string]any{"type": "text", "text": "from the family PC"}}},
			})
			_ = conn.Write(ctx, websocket.MessageText, reply)
		}
	}()

	// Wait for the link to register.
	for deadline := time.Now().Add(5 * time.Second); !server.bodyLink.Connected(); {
		if time.Now().After(deadline) {
			t.Fatal("link never registered")
		}
		time.Sleep(20 * time.Millisecond)
	}

	// The guard must accept a connected Body without any tunnel.
	if err := server.ensureTunnelOr503(); err != nil {
		t.Fatalf("guard: %v", err)
	}

	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/body/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":"x1","method":"tools/call","params":{}}`))
	req.Header.Set("Authorization", "Bearer "+server.cfg.BodyToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("proxy status %d", resp.StatusCode)
	}
	var decoded map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&decoded); err != nil {
		t.Fatal(err)
	}
	result, _ := decoded["result"].(map[string]any)
	content, _ := result["content"].([]any)
	if len(content) == 0 {
		t.Fatalf("no content: %v", decoded)
	}
	first, _ := content[0].(map[string]any)
	if first["text"] != "from the family PC" {
		t.Fatalf("reply = %v", decoded)
	}
}
