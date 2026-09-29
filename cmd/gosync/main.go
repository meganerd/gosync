package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	checksums "github.com/gbjohnso/gosync/pkg/checksum"
	"github.com/gbjohnso/gosync/pkg/deploy"
	"github.com/gbjohnso/gosync/pkg/ranged"
	"github.com/gbjohnso/gosync/pkg/server"
	"github.com/gbjohnso/gosync/pkg/sync"
	"github.com/gbjohnso/gosync/pkg/transport"
)

var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "serve" {
		runServe()
		return
	}

	var (
		workers       = flag.Int("workers", 0, "Number of parallel workers (0 = adaptive)")
		transportType = flag.String("transport", "quic", "File-transfer protocol: quic (UDP), tcp, ssh, server; -deploy supports quic/tcp/server and always uses SSH for setup")
		bufferSize    = flag.Int("buffer-size", checksums.DefaultBufferSize, "QUIC/TCP/server copy buffer size in bytes (4096–4194304); four buffers with -checksum, one without; -deploy sets both endpoints")
		connections   = flag.Int("connections", ranged.DefaultConnections, "QUIC data connections (sender sockets) per file (1–16); defaults to 4 for QUIC and 1 for other transports; fan-out is skipped for files below 64 MiB")
		bandwidth     = flag.Int64("bandwidth", 0, "Bandwidth limit in bytes/sec (0 = unlimited)")
		compression   = flag.Bool("compress", false, "Enable compression")
		dryRun        = flag.Bool("dry-run", false, "Show what would be transferred")
		verbose       = flag.Bool("verbose", false, "Show detailed transfer info")
		quiet         = flag.Bool("quiet", false, "Suppress non-error output")
		progress      = flag.Bool("progress", true, "Show overall transfer progress on stderr (disabled by -quiet or -dry-run)")
		resume        = flag.Bool("resume", false, "Skip already-transferred files")
		checksum      = flag.Bool("checksum", false, "Verify application SHA-256 on QUIC/TCP/server transfers (default off; QUIC TLS remains enabled)")
		showVersion   = flag.Bool("version", false, "Show version info")
		deployFlag    = flag.Bool("deploy", false, "Deploy via SSH, then transfer files using the selected quic/tcp/server transport")
		certFile      = flag.String("cert", "", "Local public server certificate PEM to pin for QUIC")
		remoteCert    = flag.String("remote-cert", "", "Existing remote certificate PEM path (QUIC -deploy; requires -remote-key)")
		remoteKey     = flag.String("remote-key", "", "Existing remote private-key path (QUIC -deploy; key stays remote)")
		deployKey     = flag.String("deploy-key", "", "SSH key file for deployment")
		deployListen  = flag.String("deploy-listen", "0.0.0.0:0", "Listen address for remote server (port 0 = auto-select)")
		excludes      = flag.String("exclude", "", "Exclude patterns (comma-separated)")
		includes      = flag.String("include", "", "Include patterns (comma-separated)")
	)

	flag.BoolVar(progress, "P", true, "Alias for -progress only; does not enable partial retention or resume")

	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: gosync [options] <source> <destination>\n\n")
		fmt.Fprintf(os.Stderr, "Adaptive parallel file transfer tool\n\n")
		fmt.Fprintf(os.Stderr, "Subcommands:\n")
		fmt.Fprintf(os.Stderr, "  serve     Run as a server listening for incoming transfers\n\n")
		fmt.Fprintf(os.Stderr, "Options:\n")
		flag.PrintDefaults()
		fmt.Fprintln(os.Stderr, "\nTransport notes:")
		fmt.Fprintln(os.Stderr, "  -transport selects actual file transfer, not SSH deployment.")
		fmt.Fprintln(os.Stderr, "  gosync serve supports -transport quic (UDP) or tcp; its default remains tcp.")
		fmt.Fprintln(os.Stderr, "  -deploy uses SSH for setup and honors -transport for file data (quic by default).")
		fmt.Fprintf(os.Stderr, "\nExamples:\n")
		fmt.Fprintf(os.Stderr, "  gosync -transport quic -workers 8 /path/to/source user@host:/path/to/dest\n")
		fmt.Fprintf(os.Stderr, "  gosync -transport ssh -resume /music/ user@server:/mnt/music/\n")
		fmt.Fprintf(os.Stderr, "  gosync -dry-run -verbose /data/ user@host:/backup/\n")
		fmt.Fprintf(os.Stderr, "  gosync -exclude '*.tmp,*.log' /src/ user@host:/dst/\n")
		fmt.Fprintf(os.Stderr, "  gosync -deploy /path/to/source user@host:/data/\n")
		fmt.Fprintf(os.Stderr, "  gosync -transport quic -connections 4 /path/to/big.iso host:9444:/big.iso\n")
		fmt.Fprintf(os.Stderr, "  gosync serve --listen 0.0.0.0:8443 --base /data\n")
	}

	flag.Parse()

	if *showVersion {
		fmt.Printf("gosync %s (commit: %s, built: %s)\n", version, commit, date)
		os.Exit(0)
	}

	if flag.NArg() < 2 {
		fmt.Fprintf(os.Stderr, "Error: source and destination required\n\n")
		flag.Usage()
		os.Exit(1)
	}

	if err := checksums.ValidateBufferSize(*bufferSize); err != nil {
		fmt.Fprintf(os.Stderr, "Error: -buffer-size: %v\n", err)
		os.Exit(1)
	}
	bufferExplicit := flagWasSet("buffer-size")
	if bufferExplicit && *transportType == "ssh" {
		fmt.Fprintln(os.Stderr, "Error: -buffer-size applies only to QUIC/TCP/server transfers")
		os.Exit(1)
	}

	if *checksum && *transportType == "ssh" {
		fmt.Fprintln(os.Stderr, "Error: -checksum applies only to QUIC/TCP/server transfers; SSH transport does not support application SHA-256 verification")
		os.Exit(1)
	}

	if *connections < 1 || *connections > ranged.MaxConnections {
		fmt.Fprintf(os.Stderr, "Error: -connections must be between 1 and %d\n", ranged.MaxConnections)
		os.Exit(1)
	}
	connectionsExplicit := flagWasSet("connections")
	if *connections > 1 && *transportType != "quic" && connectionsExplicit {
		fmt.Fprintln(os.Stderr, "Error: -connections above 1 applies only to QUIC data transfer; multi-socket fan-out is not implemented for tcp, server or ssh")
		os.Exit(1)
	}
	if *transportType != "quic" && !connectionsExplicit {
		*connections = 1
	}

	tlsOptions := transferTLSOptions{certFile: *certFile, remoteCert: *remoteCert, remoteKey: *remoteKey}
	certificatePEM, err := tlsOptions.validate(*transportType, *deployFlag)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}

	source := flag.Arg(0)
	destination := flag.Arg(1)

	if *deployFlag {
		runDeploy(source, destination, *deployKey, *deployListen, *transportType, *workers, *bandwidth, *bufferSize, *connections, *compression, *dryRun, *verbose, *quiet, *progress, *resume, *checksum, tlsOptions, certificatePEM)
		return
	}

	if _, err := os.Stat(source); os.IsNotExist(err) {
		fmt.Fprintf(os.Stderr, "Error: source path does not exist: %s\n", source)
		os.Exit(1)
	}

	config := transport.Config{
		CertificatePEM: certificatePEM,
		Host:           extractHost(destination),
		Port:           extractPort(destination),
		Username:       extractUser(destination),
		Timeout:        30,
		MaxRetries:     3,
		Compression:    *compression,
		Bandwidth:      *bandwidth,
		BufferSize:     *bufferSize,
		Connections:    *connections,
		Checksum:       *checksum,
	}

	transportImpl, err := newTransferTransport(*transportType, config)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}

	reportFileTransferTransport(os.Stdout, *transportType, false, *dryRun, *quiet)

	var excludeList, includeList []string
	if *excludes != "" {
		excludeList = strings.Split(*excludes, ",")
	}
	if *includes != "" {
		includeList = strings.Split(*includes, ",")
	}

	syncConfig := sync.Config{
		Workers:     *workers,
		Connections: *connections,
		Bandwidth:   *bandwidth,
		Compression: *compression,
		DryRun:      *dryRun,
		Verbose:     *verbose,
		Quiet:       *quiet,
		Progress:    *progress,
		Resume:      *resume,
		Checksum:    *checksum,
		Excludes:    excludeList,
		Includes:    includeList,
	}

	s := sync.NewSync(source, destination, transportImpl, syncConfig)
	if err := s.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}

func newTransferTransport(protocol string, config transport.Config) (transport.Transport, error) {
	switch protocol {
	case "quic":
		return transport.NewQUICTransport(config), nil
	case "tcp":
		return transport.NewTCPTransport(config), nil
	case "server":
		return transport.NewServerTransport(config), nil
	case "ssh":
		return transport.NewSSHTransport(config), nil
	default:
		return nil, fmt.Errorf("unsupported transport: %s", protocol)
	}
}

// reportFileTransferTransport describes configuration, not a negotiated connection.
func reportFileTransferTransport(out io.Writer, selected string, deployment, dryRun, quiet bool) {
	if quiet {
		return
	}

	effective := selected
	var protocol string
	switch effective {
	case "quic":
		protocol = "UDP"
	case "tcp", "server":
		protocol = "TCP"
	case "ssh":
		protocol = "SSH over TCP"
	default:
		return
	}

	fmt.Fprintf(out, "File-transfer transport: %s (%s)", effective, protocol)
	if deployment {
		fmt.Fprint(out, "; deployment: SSH")

	}
	if dryRun {
		fmt.Fprintln(out, "; planned; dry-run")
	} else {
		fmt.Fprintln(out, "; selected; not yet connected")
	}
}

func runDeploy(source, destination, keyFile, listenAddr, transportType string, workers int, bandwidth int64, bufferSize, connections int, compression, dryRun, verbose, quiet, progress, resume, checksum bool, tlsOptions transferTLSOptions, certificatePEM []byte) {
	reportFileTransferTransport(os.Stdout, transportType, true, dryRun, quiet)
	d := deploy.NewDeployer(source, destination, keyFile)
	if err := d.SetTransport(transportType); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
	if err := d.SetBufferSize(bufferSize); err != nil {
		fmt.Fprintf(os.Stderr, "Error: -buffer-size: %v\n", err)
		os.Exit(1)
	}
	if err := d.SetConnections(connections); err != nil {
		fmt.Fprintf(os.Stderr, "Error: -connections: %v\n", err)
		os.Exit(1)
	}

	if err := d.SetRemoteTLSFiles(tlsOptions.remoteCert, tlsOptions.remoteKey); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}

	if idx := strings.LastIndex(listenAddr, ":"); idx != -1 {
		if p, err := strconv.Atoi(listenAddr[idx+1:]); err == nil && p > 0 && p < 65536 {
			d.SetServerPort(p)
		}
	}

	if err := d.Deploy(); err != nil {
		fmt.Fprintf(os.Stderr, "Deploy failed: %v\n", err)
		os.Exit(1)
	}

	if len(certificatePEM) == 0 {
		certificatePEM = d.CertificatePEM()
	}
	serverPort := d.GetServerPort()

	if !quiet {
		fmt.Printf("Deployed gosync to %s\n", d.GetHost())
		fmt.Printf("Remote server listening on %s:%d\n", d.GetHost(), serverPort)
	}

	config := transport.Config{
		CertificatePEM: certificatePEM,
		Host:           d.GetHost(),
		Port:           serverPort,
		Username:       d.GetUsername(),
		Timeout:        30,
		MaxRetries:     3,
		Compression:    compression,
		Bandwidth:      bandwidth,
		BufferSize:     bufferSize,
		Connections:    connections,
		Checksum:       checksum,
	}

	transportImpl, err := newTransferTransport(transportType, config)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		d.Cleanup()
		os.Exit(1)
	}

	var excludeList, includeList []string
	syncConfig := sync.Config{
		Workers:     workers,
		Connections: connections,
		Bandwidth:   bandwidth,
		Compression: compression,
		DryRun:      dryRun,
		Verbose:     verbose,
		Quiet:       quiet,
		Progress:    progress,
		Resume:      resume,
		Checksum:    checksum,
		Excludes:    excludeList,
		Includes:    includeList,
	}

	deployDest := fmt.Sprintf("%s@%s:%d", d.GetUsername(), d.GetHost(), serverPort)
	s := sync.NewSync(source, deployDest, transportImpl, syncConfig)
	s.SetRemoteBase(d.GetBaseDir())
	if err := s.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "Sync failed: %v\n", err)
		d.Cleanup()
		os.Exit(1)
	}

	d.Cleanup()
}

func runServe() {
	serveCmd := flag.NewFlagSet("serve", flag.ExitOnError)
	listenAddr := serveCmd.String("listen", "0.0.0.0:9444", "Listen address")
	baseDir := serveCmd.String("base", "/tmp/gosync-receive", "Base directory for received files")
	transportType := serveCmd.String("transport", "tcp", "Receiver protocol: tcp or quic (UDP with TLS)")
	bufferSize := serveCmd.Int("buffer-size", checksums.DefaultBufferSize, "Copy buffer size in bytes (4096–4194304); four buffers with checksums, one without")
	connections := serveCmd.Int("connections", ranged.DefaultConnections, "QUIC receive sockets bound to the listen port with SO_REUSEPORT (1–16); defaults to 4 for QUIC and 1 for TCP")

	certFile := serveCmd.String("cert", "", "QUIC certificate PEM path (requires -key)")
	keyFile := serveCmd.String("key", "", "QUIC private-key path (requires -cert)")
	certOut := serveCmd.String("cert-out", "", "Export QUIC public leaf PEM to a new file; never exports private keys")

	serveCmd.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: gosync serve [options]\n\n")
		fmt.Fprintf(os.Stderr, "Run as a server listening for incoming transfers\n\n")
		fmt.Fprintf(os.Stderr, "Options:\n")
		serveCmd.PrintDefaults()
	}

	serveCmd.Parse(os.Args[2:])
	connectionsExplicit := flagSetWasSet(serveCmd, "connections")
	if *transportType != "quic" && !connectionsExplicit {
		*connections = 1
	}

	srv := server.NewServer(*listenAddr, *baseDir)
	if err := srv.SetTransport(*transportType); err != nil {
		fmt.Fprintf(os.Stderr, "Error: -transport: %v\n", err)
		os.Exit(1)
	}
	if err := srv.SetBufferSize(*bufferSize); err != nil {
		fmt.Fprintf(os.Stderr, "Error: -buffer-size: %v\n", err)
		os.Exit(1)
	}
	if err := configureServerConnections(srv, *connections, *transportType); err != nil {
		fmt.Fprintf(os.Stderr, "Error: -connections: %v\n", err)
		os.Exit(1)
	}
	if err := srv.SetTLSFiles(*certFile, *keyFile); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
	if err := srv.SetCertificateOutput(*certOut); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
	if err := srv.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "Server error: %v\n", err)
		os.Exit(1)
	}
}

// configureServerConnections sizes the receiver's SO_REUSEPORT socket group.
// The setter is discovered through an interface so this flag plumbing stays
// decoupled from the server package's concrete type.
func configureServerConnections(srv any, connections int, transportType string) error {
	if connections < 1 || connections > ranged.MaxConnections {
		return fmt.Errorf("must be between 1 and %d", ranged.MaxConnections)
	}
	if connections > 1 && transportType != "quic" {
		return fmt.Errorf("multi-socket receive applies only to -transport quic")
	}
	if configurable, ok := srv.(interface{ SetMaxConnections(int) error }); ok {
		return configurable.SetMaxConnections(connections)
	}
	if connections > 1 {
		return fmt.Errorf("this receiver build does not support multi-socket receive")
	}
	return nil
}

func flagWasSet(name string) bool {
	return flagSetWasSet(flag.CommandLine, name)
}

func flagSetWasSet(flags *flag.FlagSet, name string) bool {
	set := false
	flags.Visit(func(f *flag.Flag) { set = set || f.Name == name })
	return set
}

func extractHost(dest string) string {
	if idx := strings.Index(dest, "@"); idx != -1 {
		dest = dest[idx+1:]
	}
	if idx := strings.Index(dest, ":"); idx != -1 {
		return dest[:idx]
	}
	return dest
}

func extractPort(dest string) int {
	if idx := strings.Index(dest, "@"); idx != -1 {
		dest = dest[idx+1:]
	}
	if idx := strings.Index(dest, ":"); idx != -1 {
		var port int
		fmt.Sscanf(dest[idx+1:], "%d", &port)
		if port > 0 {
			return port
		}
		return 22
	}
	return 22
}

func extractUser(dest string) string {
	if idx := strings.Index(dest, "@"); idx != -1 {
		return dest[:idx]
	}
	return ""
}
