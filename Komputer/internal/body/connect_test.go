package body

import "testing"

// The connect target accepts the API base URL a person would paste, not only
// a finished WebSocket URL.
func TestConnectURLNormalization(t *testing.T) {
	cases := map[string]string{
		"https://brain.example":            "wss://brain.example/body/connect",
		"http://127.0.0.1:18888":           "ws://127.0.0.1:18888/body/connect",
		"wss://brain.example/body/connect": "wss://brain.example/body/connect",
		"https://brain.example/prefix":     "wss://brain.example/prefix/body/connect",
		" https://brain.example/ ":         "wss://brain.example/body/connect",
	}
	for in, want := range cases {
		got, err := connectURL(in)
		if err != nil {
			t.Errorf("connectURL(%q): %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("connectURL(%q) = %q, want %q", in, got, want)
		}
	}
	if _, err := connectURL("ftp://brain.example"); err == nil {
		t.Fatal("ftp scheme must be rejected")
	}
	if _, err := connectURL(""); err == nil {
		t.Fatal("empty URL must be rejected")
	}
}
