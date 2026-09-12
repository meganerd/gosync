package transport

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"fmt"
)

// ValidateCertificatePEM validates a QUIC leaf pin followed by optional chain
// certificates. Only headerless CERTIFICATE PEM blocks and whitespace are
// accepted, never private keys. Empty data selects system PKI. Certificate
// validity and server-auth usage are checked at connection time, not here.
func ValidateCertificatePEM(data []byte) error {
	_, err := parseCertificatePEM(data)
	return err
}

func parseCertificatePEM(data []byte) (*x509.Certificate, error) {
	if len(data) == 0 {
		return nil, nil
	}
	var leaf *x509.Certificate
	for rest := bytes.TrimSpace(data); len(rest) > 0; {
		// Decode one block at a time so pem.Decode cannot silently skip junk or
		// a malformed block preceding a valid certificate.
		const begin = "-----BEGIN CERTIFICATE-----"
		const end = "-----END CERTIFICATE-----"
		if !bytes.HasPrefix(rest, []byte(begin)) {
			return nil, fmt.Errorf("expected CERTIFICATE PEM block (private keys are not allowed)")
		}
		i := bytes.Index(rest, []byte(end))
		if i < 0 {
			return nil, fmt.Errorf("unterminated CERTIFICATE PEM block")
		}
		n := i + len(end)
		if bytes.Count(rest[:n], []byte("-----BEGIN ")) != 1 {
			return nil, fmt.Errorf("nested or malformed PEM block")
		}
		block, remainder := pem.Decode(rest[:n])
		if block == nil || block.Type != "CERTIFICATE" || len(block.Headers) != 0 || len(remainder) != 0 {
			return nil, fmt.Errorf("invalid CERTIFICATE PEM block")
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("parse certificate: %w", err)
		}
		if leaf == nil {
			leaf = cert
		}
		rest = bytes.TrimSpace(rest[n:])
	}
	if leaf == nil {
		return nil, fmt.Errorf("no certificate in PEM data")
	}
	return leaf, nil
}

func quicClientTLSConfig(host string, data []byte) (*tls.Config, error) {
	leaf, err := parseCertificatePEM(data)
	if err != nil {
		return nil, err
	}
	config := &tls.Config{
		ServerName: host,
		MinVersion: tls.VersionTLS13,
		NextProtos: []string{"gosync"},
	}
	if leaf == nil {
		return config, nil
	}
	// The exact leaf is the trust anchor, not its issuer or its hostname.
	// VerifyConnection also runs on resumed connections, unlike VerifyPeerCertificate.
	config.InsecureSkipVerify = true
	config.VerifyConnection = func(state tls.ConnectionState) error {
		if len(state.PeerCertificates) == 0 || !bytes.Equal(state.PeerCertificates[0].Raw, leaf.Raw) {
			return fmt.Errorf("QUIC server certificate does not match pinned leaf")
		}
		roots := x509.NewCertPool()
		roots.AddCert(leaf)
		_, err := state.PeerCertificates[0].Verify(x509.VerifyOptions{
			Roots:     roots,
			KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		})
		if err != nil {
			return fmt.Errorf("verify pinned QUIC server certificate: %w", err)
		}
		return nil
	}
	return config, nil
}
