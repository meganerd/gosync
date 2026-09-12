package main

import (
	"fmt"
	"os"

	"github.com/gbjohnso/gosync/pkg/transport"
)

type transferTLSOptions struct {
	certFile   string
	remoteCert string
	remoteKey  string
}

// Validate before deployment so invalid credentials cannot trigger remote setup.
func (o transferTLSOptions) validate(protocol string, deployment bool) ([]byte, error) {
	if protocol != "quic" && (o.certFile != "" || o.remoteCert != "" || o.remoteKey != "") {
		return nil, fmt.Errorf("certificate options apply only to QUIC")
	}
	if (o.remoteCert == "") != (o.remoteKey == "") {
		return nil, fmt.Errorf("-remote-cert and -remote-key must be provided together")
	}
	if !deployment && o.remoteCert != "" {
		return nil, fmt.Errorf("-remote-cert and -remote-key require -deploy; use -cert to pin an existing receiver")
	}
	if deployment && o.certFile != "" && o.remoteCert == "" {
		return nil, fmt.Errorf("-deploy -cert requires -remote-cert and -remote-key; otherwise deployment generates and pins a new certificate automatically")
	}
	if o.certFile == "" {
		return nil, nil
	}
	data, err := os.ReadFile(o.certFile)
	if err != nil {
		return nil, fmt.Errorf("read -cert: %w", err)
	}
	if len(data) == 0 {
		return nil, fmt.Errorf("-cert file is empty")
	}
	if err := transport.ValidateCertificatePEM(data); err != nil {
		return nil, fmt.Errorf("invalid -cert: %w", err)
	}
	return data, nil
}
