package api

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// An encrypted PKCS#8 key (what openssl genpkey writes by default) must load
// both with and without the configured passphrase.
func TestLoadKeyPairWithPassphrase(t *testing.T) {
	if _, err := exec.LookPath("openssl"); err != nil {
		t.Skip("openssl not installed")
	}
	dir := t.TempDir()
	cert := filepath.Join(dir, "cert.pem")
	key := filepath.Join(dir, "key.pem")
	run := func(name string, args ...string) {
		t.Helper()
		cmd := exec.Command(name, args...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("%s: %v: %s", name, err, out)
		}
	}
	run("openssl", "req", "-x509", "-newkey", "rsa:2048", "-keyout", key, "-out", cert,
		"-days", "1", "-nodes", "-subj", "/CN=localhost")
	run("openssl", "pkey", "-in", key, "-out", key+".enc", "-aes-256-cbc", "-passout", "pass:secret")
	if err := os.Rename(key+".enc", key); err != nil {
		t.Fatal(err)
	}
	if _, err := loadKeyPair(cert, key, "wrong"); err == nil {
		t.Fatal("wrong passphrase accepted")
	}
	pair, err := loadKeyPair(cert, key, "secret")
	if err != nil {
		t.Fatalf("correct passphrase: %v", err)
	}
	if len(pair.Certificate) == 0 || pair.PrivateKey == nil {
		t.Fatal("incomplete key pair")
	}
	// An unencrypted key must still load with no passphrase configured.
	run("openssl", "pkey", "-in", key, "-out", key+".plain", "-passin", "pass:secret")
	if err := os.Rename(key+".plain", key); err != nil {
		t.Fatal(err)
	}
	if _, err := loadKeyPair(cert, key, ""); err != nil {
		t.Fatalf("plain key: %v", err)
	}
}
