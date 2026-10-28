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
func TestConnectURLRejectsPublicPlaintext(t *testing.T) {
	if _, err := connectURL("http://brain.example:18888"); err == nil {
		t.Fatal("public plaintext http accepted")
	}
	if _, err := connectURL("ws://8.8.8.8:9000"); err == nil {
		t.Fatal("public plaintext ws accepted")
	}
	for _, ok := range []string{"http://127.0.0.1:18888", "ws://10.0.0.5:9000", "http://localhost:18888"} {
		if _, err := connectURL(ok); err != nil {
			t.Errorf("private plaintext %q rejected: %v", ok, err)
		}
	}
	t.Setenv("KLAUD_KODE_BRAIN_ALLOW_PLAINTEXT", "1")
	if _, err := connectURL("http://brain.example:18888"); err != nil {
		t.Errorf("explicit opt-in rejected: %v", err)
	}
}

func TestConnectURLPathSuffix(t *testing.T) {
	got, err := connectURL("https://brain.example/body/connect/")
	if err != nil {
		t.Fatal(err)
	}
	if got != "wss://brain.example/body/connect" {
		t.Fatalf("got %q", got)
	}
	if _, err := connectURL("https:///no-host"); err == nil {
		t.Fatal("empty host accepted")
	}
}
