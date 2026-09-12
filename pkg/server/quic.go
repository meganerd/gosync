package server

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"fmt"
	"math/big"
	"net"
	"time"

	"github.com/quic-go/quic-go"
)

func listenQUIC(addr string, config *tls.Config) (*quic.Listener, error) {
	return quic.ListenAddr(addr, config, &quic.Config{})
}

// quicGroup is one or more UDP sockets bound to the same port, each with its
// own quic.Listener over one shared tls.Config and certificate. Measurement
// put receiver socket count at ~9-11% of fan-out's gain, so the receiver keeps
// a single advertised port and shares it with SO_REUSEPORT rather than opening
// more ports. See docs/multi-connection-quic-design.md.
type quicGroup struct {
	listeners  []*quic.Listener
	transports []*quic.Transport
	sockets    []*net.UDPConn
}

func (g *quicGroup) close() {
	for _, listener := range g.listeners {
		listener.Close()
	}
	for _, transport := range g.transports {
		transport.Close()
	}
	// quic.Transport.Close does not close a caller-supplied connection.
	for _, socket := range g.sockets {
		socket.Close()
	}
}

// listenQUICGroup binds count sockets to a single port. count <= 1 takes the
// original single-listener path untouched, so the default is byte-for-byte
// today's behavior. A receiver that cannot set SO_REUSEPORT logs and falls back
// to one socket rather than failing to start.
func listenQUICGroup(addr string, config *tls.Config, count int) (*quicGroup, error) {
	if count <= 1 {
		listener, err := listenQUIC(addr, config)
		if err != nil {
			return nil, err
		}
		return &quicGroup{listeners: []*quic.Listener{listener}}, nil
	}
	group := &quicGroup{}
	for i := 0; i < count; i++ {
		target := addr
		if i > 0 {
			// Bind the rest to the port the kernel chose for the first socket,
			// so a ":0" request still yields one advertised port.
			target = group.sockets[0].LocalAddr().String()
		}
		socket, err := listenReusePortUDP(target)
		if err == nil {
			transport := &quic.Transport{Conn: socket}
			var listener *quic.Listener
			listener, err = transport.Listen(config, &quic.Config{})
			if err == nil {
				group.sockets = append(group.sockets, socket)
				group.transports = append(group.transports, transport)
				group.listeners = append(group.listeners, listener)
				continue
			}
			transport.Close()
			socket.Close()
		}
		if i == 0 {
			fmt.Printf("gosync: SO_REUSEPORT socket group unavailable (%v); using a single QUIC socket\n", err)
			listener, fallbackErr := listenQUIC(addr, config)
			if fallbackErr != nil {
				return nil, fallbackErr
			}
			return &quicGroup{listeners: []*quic.Listener{listener}}, nil
		}
		// Partial groups are usable: every socket serves identical sessions.
		fmt.Printf("gosync: bound %d of %d QUIC sockets (%v)\n", i, count, err)
		break
	}
	return group, nil
}

func listenReusePortUDP(addr string) (*net.UDPConn, error) {
	config := net.ListenConfig{Control: setReusePort}
	packetConn, err := config.ListenPacket(context.Background(), "udp", addr)
	if err != nil {
		return nil, err
	}
	socket, ok := packetConn.(*net.UDPConn)
	if !ok {
		packetConn.Close()
		return nil, fmt.Errorf("expected *net.UDPConn, got %T", packetConn)
	}
	return socket, nil
}

// ephemeralTLSConfig generates an in-memory identity that clients can authenticate
// by pinning its public certificate. The private key is never written to disk.
func ephemeralTLSConfig() (*tls.Config, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, err
	}
	now := time.Now()
	template := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "gosync ephemeral"},
		NotBefore:    now.Add(-time.Minute), NotAfter: now.Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}
	return &tls.Config{
		MinVersion:   tls.VersionTLS13,
		NextProtos:   []string{"gosync"},
		Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}},
	}, nil
}

// acceptExtraQUIC serves the remaining sockets of a SO_REUSEPORT group while
// Start blocks on the first. Each loop is a tracked worker, so Stop closing the
// listeners makes them all return before Stop finishes waiting.
func (s *Server) acceptExtraQUIC(listeners []*quic.Listener) {
	for _, listener := range listeners {
		s.mu.Lock()
		if s.stopped {
			s.mu.Unlock()
			return
		}
		s.workers.Add(1)
		s.mu.Unlock()
		go func(listener *quic.Listener) {
			defer s.workers.Done()
			if err := s.acceptQUIC(listener); err != nil {
				fmt.Printf("gosync: QUIC accept loop on %s stopped: %v\n", listener.Addr(), err)
			}
		}(listener)
	}
}

func (s *Server) acceptQUIC(listener *quic.Listener) error {
	for {
		conn, err := listener.Accept(context.Background())
		if err != nil {
			return s.acceptError(err)
		}
		s.mu.Lock()
		if s.stopped {
			s.mu.Unlock()
			conn.CloseWithError(0, "server stopped")
			return nil
		}
		s.quicConns[conn] = struct{}{}
		s.workers.Add(1)
		s.mu.Unlock()
		go s.serveQUIC(conn)
	}
}

func (s *Server) serveQUIC(conn *quic.Conn) {
	defer s.workers.Done()
	defer func() {
		conn.CloseWithError(0, "session closed")
		s.mu.Lock()
		delete(s.quicConns, conn)
		s.mu.Unlock()
	}()
	for {
		stream, err := conn.AcceptStream(context.Background())
		if err != nil {
			return
		}
		s.serveClient(&quicStreamConn{Stream: stream, conn: conn})
	}
}

// quicStreamConn gives the shared protocol handler a net.Conn per operation.
// Stream embeds Read, Write and all three deadline methods.
type quicStreamConn struct {
	*quic.Stream
	conn *quic.Conn
}

var _ net.Conn = (*quicStreamConn)(nil)

func (c *quicStreamConn) LocalAddr() net.Addr  { return c.conn.LocalAddr() }
func (c *quicStreamConn) RemoteAddr() net.Addr { return c.conn.RemoteAddr() }
func (c *quicStreamConn) Close() error {
	// FIN preserves queued response/checksum bytes. CancelWrite would RESET the
	// outbound stream and could discard those bytes before the peer reads them.
	err := c.Stream.Close()
	c.Stream.CancelRead(0)
	return err
}
