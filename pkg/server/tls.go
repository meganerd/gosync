package server

import (
	"crypto/tls"
	"encoding/pem"
	"fmt"
	"io"
	"os"
)

// SetTLSFiles selects a certificate chain and private key to load before listening.
// Both paths are required, or both may be empty to select a generated certificate.
// Configure it before Start. Supplied files are only ever read.
func (s *Server) SetTLSFiles(certFile, keyFile string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.started || s.stopped {
		return fmt.Errorf("TLS files must be configured before Start")
	}
	if (certFile == "") != (keyFile == "") {
		return fmt.Errorf("TLS certificate and key files must be provided together")
	}
	s.certFile, s.keyFile = certFile, keyFile
	return nil
}

// SetCertificateOutput selects an optional new file for the public leaf PEM.
// Configure it before Start; an empty path disables export. Existing files and
// symlinks are refused, and the private key is never exported.
func (s *Server) SetCertificateOutput(path string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.started || s.stopped {
		return fmt.Errorf("certificate output must be configured before Start")
	}
	s.certOutput = path
	return nil
}

// CertificatePEM returns a defensive copy of the selected public leaf certificate.
// It is available when Addr becomes non-nil, and nil before readiness or for TCP.
func (s *Server) CertificatePEM() []byte {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return append([]byte(nil), s.certificatePEM...)
}

// startQUIC holds s.mu throughout setup; neither Addr nor CertificatePEM can
// expose a partially initialized server. Bind before publishing the output so
// an SSH caller discovering the file can immediately connect.
func (s *Server) startQUIC() error {
	var config *tls.Config
	if s.certFile != "" {
		cert, err := tls.LoadX509KeyPair(s.certFile, s.keyFile)
		if err != nil {
			return fmt.Errorf("load TLS certificate/key pair: %w", err)
		}
		config = &tls.Config{
			MinVersion:   tls.VersionTLS13,
			NextProtos:   []string{"gosync"},
			Certificates: []tls.Certificate{cert},
		}
	} else {
		var err error
		config, err = ephemeralTLSConfig()
		if err != nil {
			return fmt.Errorf("generate TLS certificate: %w", err)
		}
	}
	public := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: config.Certificates[0].Certificate[0]})
	connections := s.maxConnections
	if connections < 1 {
		connections = 1
	}
	// One port, one certificate, one tls.Config, shared by every socket in the
	// group, so Addr and CertificatePEM keep their single-listener meaning.
	group, err := listenQUICGroup(s.listenAddr, config, connections)
	if err != nil {
		return err
	}
	if s.certOutput != "" {
		if err := exportCertificate(s.certOutput, public); err != nil {
			group.close()
			return fmt.Errorf("export public certificate: %w", err)
		}
	}
	s.certificatePEM = public
	s.quicGroup = group
	s.quicListener = group.listeners[0]
	return nil
}

func exportCertificate(path string, public []byte) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	info, statErr := file.Stat()
	n, writeErr := file.Write(public)
	if writeErr == nil && n != len(public) {
		writeErr = io.ErrShortWrite
	}
	closeErr := file.Close()
	if statErr == nil && writeErr == nil && closeErr == nil {
		return nil
	}
	// Do not remove a replacement installed by someone else during a failed write.
	if current, err := os.Lstat(path); err == nil && info != nil && os.SameFile(info, current) {
		if err := os.Remove(path); err != nil {
			return fmt.Errorf("certificate export failed (%v, %v, %v); cleanup: %w", statErr, writeErr, closeErr, err)
		}
	}
	if statErr != nil {
		return statErr
	}
	if writeErr != nil {
		return writeErr
	}
	return closeErr
}
