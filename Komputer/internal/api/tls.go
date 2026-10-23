package api

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"os"

	"github.com/youmark/pkcs8"
)

// loadKeyPair loads the certificate chain and key, decrypting an encrypted
// PKCS#8 key when a passphrase is configured. tls.LoadX509KeyPair cannot do
// that, and openssl writes encrypted PKCS#8 by default.
func loadKeyPair(certFile, keyFile, passphrase string) (tls.Certificate, error) {
	if passphrase == "" {
		return tls.LoadX509KeyPair(certFile, keyFile)
	}
	certPEM, err := os.ReadFile(certFile)
	if err != nil {
		return tls.Certificate{}, err
	}
	keyPEM, err := os.ReadFile(keyFile)
	if err != nil {
		return tls.Certificate{}, err
	}
	block, _ := pem.Decode(keyPEM)
	if block == nil {
		return tls.Certificate{}, fmt.Errorf("%s is not PEM", keyFile)
	}
	// ParsePKCS8PrivateKey takes DER, not the PEM armor.
	key, err := pkcs8.ParsePKCS8PrivateKey(block.Bytes, []byte(passphrase))
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("cannot decrypt %s with KOMPUTER_TLS_KEY_PASSPHRASE: %w", keyFile, err)
	}
	pemBlock, err := marshalPrivateKey(key)
	if err != nil {
		return tls.Certificate{}, err
	}
	return tls.X509KeyPair(certPEM, pemBlock)
}

// marshalPrivateKey re-encodes a parsed key as unencrypted PKCS#8 PEM, which
// X509KeyPair can read.
func marshalPrivateKey(key any) ([]byte, error) {
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), nil
}
