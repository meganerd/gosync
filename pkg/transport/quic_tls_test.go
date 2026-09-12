package transport

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/quic-go/quic-go"
)

func pinnedTestCertificate(t *testing.T, modify func(*x509.Certificate)) (tls.Certificate, []byte, *x509.Certificate) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		NotBefore:    time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage:    x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:    []string{"server.example"},
	}
	if modify != nil {
		modify(template)
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), leaf
}

func TestValidateCertificatePEM(t *testing.T) {
	_, cert, _ := pinnedTestCertificate(t, nil)
	_, chain, _ := pinnedTestCertificate(t, nil)
	privateKey := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: []byte("secret")})
	header := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Headers: map[string]string{"Foo": "bar"}, Bytes: []byte("invalid")})
	for _, tc := range []struct {
		name  string
		data  []byte
		valid bool
	}{
		{"empty", nil, true},
		{"leaf", cert, true},
		{"chain", append(bytes.Clone(cert), chain...), true},
		{"whitespace around leaf", append([]byte(" \n"), append(bytes.Clone(cert), ' ', '\n')...), true},
		{"whitespace only", []byte(" \n"), false},
		{"garbage", []byte("not PEM"), false},
		{"private key", privateKey, false},
		{"key after leaf", append(bytes.Clone(cert), privateKey...), false},
		{"key before leaf", append(bytes.Clone(privateKey), cert...), false},
		{"junk before leaf", append([]byte("junk\n"), cert...), false},
		{"junk after leaf", append(bytes.Clone(cert), []byte("junk")...), false},
		{"invalid DER", pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: []byte("invalid")}), false},
		{"headers", header, false},
		{"malformed before leaf", append([]byte("-----BEGIN CERTIFICATE-----\n!!!\n-----END CERTIFICATE-----\n"), cert...), false},
		{"truncated before leaf", append([]byte("-----BEGIN CERTIFICATE-----\n!!!\n"), cert...), false},
		{"invalid chain", append(bytes.Clone(cert), []byte("-----BEGIN CERTIFICATE-----\n!!!\n-----END CERTIFICATE-----")...), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := ValidateCertificatePEM(tc.data); (err == nil) != tc.valid {
				t.Fatalf("ValidateCertificatePEM = %v, valid=%v", err, tc.valid)
			}
		})
	}
}

func TestQUICCertificateVerification(t *testing.T) {
	_, pin, leaf := pinnedTestCertificate(t, nil)
	_, otherPEM, other := pinnedTestCertificate(t, nil)
	config, err := quicClientTLSConfig("different.example", append(bytes.Clone(pin), otherPEM...))
	if err != nil {
		t.Fatal(err)
	}
	if !config.InsecureSkipVerify || config.VerifyConnection == nil {
		t.Fatal("pinning must pair InsecureSkipVerify with VerifyConnection")
	}
	for _, resumed := range []bool{false, true} {
		for _, tc := range []struct {
			name  string
			peers []*x509.Certificate
			valid bool
		}{
			{"exact leaf", []*x509.Certificate{leaf}, true},
			{"presented chain", []*x509.Certificate{leaf, other}, true},
			{"missing", nil, false},
			{"different leaf", []*x509.Certificate{other}, false},
			{"pin only in chain", []*x509.Certificate{other, leaf}, false},
		} {
			t.Run(tc.name, func(t *testing.T) {
				err := config.VerifyConnection(tls.ConnectionState{PeerCertificates: tc.peers, DidResume: resumed})
				if (err == nil) != tc.valid {
					t.Fatalf("VerifyConnection (resumed=%v) = %v", resumed, err)
				}
			})
		}
	}
	for _, tc := range []struct {
		name   string
		modify func(*x509.Certificate)
		valid  bool
	}{
		{"expired", func(c *x509.Certificate) { c.NotAfter = time.Now().Add(-time.Minute) }, false},
		{"future", func(c *x509.Certificate) { c.NotBefore = time.Now().Add(time.Minute) }, false},
		{"client only", func(c *x509.Certificate) { c.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth} }, false},
		{"any usage", func(c *x509.Certificate) { c.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageAny} }, true},
		{"unrestricted usage", func(c *x509.Certificate) { c.ExtKeyUsage = nil }, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, data, cert := pinnedTestCertificate(t, tc.modify)
			cfg, err := quicClientTLSConfig("server.example", data)
			if err != nil {
				t.Fatal(err)
			}
			if err := cfg.VerifyConnection(tls.ConnectionState{PeerCertificates: []*x509.Certificate{cert}}); (err == nil) != tc.valid {
				t.Fatalf("VerifyConnection = %v, valid=%v", err, tc.valid)
			}
		})
	}
}

func TestQUICSystemPKIConfig(t *testing.T) {
	config, err := quicClientTLSConfig("server.example", nil)
	if err != nil {
		t.Fatal(err)
	}
	if config.InsecureSkipVerify || config.VerifyConnection != nil || config.RootCAs != nil || config.ServerName != "server.example" {
		t.Fatal("default must use system roots and normal hostname verification")
	}
	if config.MinVersion != tls.VersionTLS13 || len(config.NextProtos) != 1 || config.NextProtos[0] != "gosync" {
		t.Fatal("incorrect QUIC TLS settings")
	}
}

func TestQUICPinCopied(t *testing.T) {
	_, data, _ := pinnedTestCertificate(t, nil)
	want := bytes.Clone(data)
	client := NewQUICTransport(Config{CertificatePEM: data})
	data[0] = '!'
	if !bytes.Equal(client.config.CertificatePEM, want) {
		t.Fatal("caller mutation changed pin")
	}
}

func TestQUICConnectCertificateAuthentication(t *testing.T) {
	cert, pin, _ := pinnedTestCertificate(t, nil)
	_, other, _ := pinnedTestCertificate(t, nil)
	for _, tc := range []struct {
		name      string
		pin       []byte
		wantError string
	}{
		{"pinned despite hostname mismatch", pin, ""},
		{"no pin rejects self signed", nil, "certificate"},
		{"wrong pin", other, "does not match pinned leaf"},
		{"invalid PEM", []byte("invalid"), "invalid QUIC certificate"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			listener, err := quic.ListenAddr("127.0.0.1:0", &tls.Config{
				Certificates: []tls.Certificate{cert}, NextProtos: []string{"gosync"},
			}, nil)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			done := make(chan struct{})
			go func() {
				defer close(done)
				conn, err := listener.Accept(ctx)
				if err != nil {
					return
				}
				defer conn.CloseWithError(0, "done")
				stream, err := conn.AcceptStream(ctx)
				if err != nil {
					return
				}
				stream.SetDeadline(time.Now().Add(3 * time.Second))
				if _, err := io.ReadAll(stream); err != nil {
					return
				}
				io.WriteString(stream, "OK PONG L3JlY2VpdmVy\n")
				stream.Close()
				select {
				case <-ctx.Done():
				case <-conn.Context().Done():
				}
			}()
			client := NewQUICTransport(Config{Checksum: true, Timeout: 2, CertificatePEM: tc.pin})
			defer func() {
				client.Close()
				cancel()
				listener.Close()
				<-done
			}()
			addr := listener.Addr().(*net.UDPAddr)
			err = client.Connect(addr.IP.String(), addr.Port)
			if tc.wantError == "" {
				if err != nil || !client.IsConnected() {
					t.Fatalf("pinned connection failed: %v", err)
				}
			} else if err == nil || !strings.Contains(err.Error(), tc.wantError) || client.IsConnected() || client.RemoteBase() != "" {
				t.Fatalf("Connect = %v, expected %q and no connection state", err, tc.wantError)
			}
		})
	}
}
