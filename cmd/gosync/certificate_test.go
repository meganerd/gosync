package main

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestTransferTLSOptionsValidateCombinations(t *testing.T) {
	missingCert := filepath.Join(t.TempDir(), "missing.pem")
	for _, tc := range []struct {
		name       string
		protocol   string
		deployment bool
		options    transferTLSOptions
		wantError  string
	}{
		{"remote cert without key", "quic", true, transferTLSOptions{remoteCert: "/remote/cert.pem"}, "-remote-cert and -remote-key must be provided together"},
		{"remote key without cert", "quic", true, transferTLSOptions{remoteKey: "/remote/key.pem"}, "-remote-cert and -remote-key must be provided together"},
		{"nondeploy remote cert without key", "quic", false, transferTLSOptions{remoteCert: "/remote/cert.pem"}, "-remote-cert and -remote-key must be provided together"},
		{"nondeploy remote key without cert", "quic", false, transferTLSOptions{remoteKey: "/remote/key.pem"}, "-remote-cert and -remote-key must be provided together"},
		{"nondeploy remote pair", "quic", false, transferTLSOptions{remoteCert: "/remote/cert.pem", remoteKey: "/remote/key.pem"}, "-remote-cert and -remote-key require -deploy"},
		{"nondeploy remote pair with local pin", "quic", false, transferTLSOptions{certFile: missingCert, remoteCert: "/remote/cert.pem", remoteKey: "/remote/key.pem"}, "-remote-cert and -remote-key require -deploy"},
		{"deploy local pin without remote pair", "quic", true, transferTLSOptions{certFile: missingCert}, "-deploy -cert requires -remote-cert and -remote-key"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tc.options.validate(tc.protocol, tc.deployment)
			if err == nil || !strings.Contains(err.Error(), tc.wantError) {
				t.Fatalf("validate() error = %v, want %q", err, tc.wantError)
			}
			if got != nil {
				t.Fatalf("validate() returned certificate %q on error", got)
			}
		})
	}
}

func TestTransferTLSOptionsValidateNonQUIC(t *testing.T) {
	for _, protocol := range []string{"tcp", "server", "ssh"} {
		t.Run(protocol, func(t *testing.T) {
			for _, deployment := range []bool{false, true} {
				for _, tc := range []struct {
					name    string
					options transferTLSOptions
				}{
					{"local cert", transferTLSOptions{certFile: "cert.pem"}},
					{"remote cert", transferTLSOptions{remoteCert: "/remote/cert.pem"}},
					{"remote key", transferTLSOptions{remoteKey: "/remote/key.pem"}},
					{"remote pair", transferTLSOptions{remoteCert: "/remote/cert.pem", remoteKey: "/remote/key.pem"}},
				} {
					t.Run(fmt.Sprintf("%s/deployment=%v", tc.name, deployment), func(t *testing.T) {
						got, err := tc.options.validate(protocol, deployment)
						if err == nil || err.Error() != "certificate options apply only to QUIC" {
							t.Fatalf("validate() error = %v, want QUIC-only error", err)
						}
						if got != nil {
							t.Fatalf("validate() returned certificate %q on error", got)
						}
					})
				}
			}
		})
	}
}

func TestTransferTLSOptionsValidateLocalCertificateErrors(t *testing.T) {
	for _, tc := range []struct {
		name      string
		data      []byte
		missing   bool
		wantError string
	}{
		{"missing", nil, true, "read -cert:"},
		{"empty", nil, false, "-cert file is empty"},
		{"whitespace", []byte(" \n\t"), false, "invalid -cert:"},
		{"malformed PEM", []byte("not a certificate"), false, "invalid -cert:"},
		{"malformed DER", pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: []byte("invalid")}), false, "invalid -cert:"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "cert.pem")
			if !tc.missing {
				if err := os.WriteFile(path, tc.data, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			for _, deployment := range []bool{false, true} {
				options := transferTLSOptions{certFile: path}
				if deployment {
					options.remoteCert = "/remote/cert.pem"
					options.remoteKey = "/remote/key.pem"
				}
				got, err := options.validate("quic", deployment)
				if err == nil || !strings.Contains(err.Error(), tc.wantError) {
					t.Fatalf("validate(deployment=%v) error = %v, want %q", deployment, err, tc.wantError)
				}
				if got != nil {
					t.Fatalf("validate(deployment=%v) returned certificate %q on error", deployment, got)
				}
				if tc.missing && !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("validate(deployment=%v) error = %v, want wrapped os.ErrNotExist", deployment, err)
				}
			}
		})
	}
}

func TestTransferTLSOptionsValidateAllowed(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{"server.example"},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	// Surrounding whitespace must survive validation, not just the parsed certificate.
	pin := append([]byte(" \n"), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})...)
	pin = append(pin, ' ', '\n')
	path := filepath.Join(t.TempDir(), "cert.pem")
	if err := os.WriteFile(path, pin, 0o600); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name       string
		protocol   string
		deployment bool
		options    transferTLSOptions
		wantPin    bool
	}{
		{"QUIC defaults", "quic", false, transferTLSOptions{}, false},
		{"automatic deploy", "quic", true, transferTLSOptions{}, false},
		{"TCP without TLS options", "tcp", false, transferTLSOptions{}, false},
		{"server without TLS options", "server", false, transferTLSOptions{}, false},
		{"SSH without TLS options", "ssh", false, transferTLSOptions{}, false},
		{"existing receiver pin", "quic", false, transferTLSOptions{certFile: path}, true},
		{"deploy remote pair without local pin", "quic", true, transferTLSOptions{remoteCert: "/remote/cert.pem", remoteKey: "/remote/key.pem"}, false},
		{"deploy preserves local pin with remote pair", "quic", true, transferTLSOptions{certFile: path, remoteCert: "/remote/cert.pem", remoteKey: "/remote/key.pem"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tc.options.validate(tc.protocol, tc.deployment)
			if err != nil {
				t.Fatalf("validate() error = %v", err)
			}
			if tc.wantPin {
				if !bytes.Equal(got, pin) {
					t.Fatalf("validate() certificate = %q, want original bytes %q", got, pin)
				}
			} else if got != nil {
				t.Fatalf("validate() certificate = %q, want nil", got)
			}
		})
	}
}
