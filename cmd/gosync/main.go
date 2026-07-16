package main

import (
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/gbjohnso/gosync/pkg/deploy"
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
		transportType = flag.String("transport", "tcp", "Transport protocol: tcp, quic, ssh")
		bandwidth     = flag.Int64("bandwidth", 0, "Bandwidth limit in bytes/sec (0 = unlimited)")
		compression   = flag.Bool("compress", false, "Enable compression")
		dryRun        = flag.Bool("dry-run", false, "Show what would be transferred")
		verbose       = flag.Bool("verbose", false, "Show detailed transfer info")
		quiet         = flag.Bool("quiet", false, "Suppress non-error output")
		resume        = flag.Bool("resume", false, "Skip already-transferred files")
		checksum      = flag.Bool("checksum", false, "Verify file integrity")
		showVersion   = flag.Bool("version", false, "Show version info")
		deployFlag    = flag.Bool("deploy", false, "Deploy gosync to remote host via SSH")
		deployKey     = flag.String("deploy-key", "", "SSH key file for deployment")
		deployListen  = flag.String("deploy-listen", "0.0.0.0:8443", "Listen address for remote server")
		excludes      = flag.String("exclude", "", "Exclude patterns (comma-separated)")
		includes      = flag.String("include", "", "Include patterns (comma-separated)")
	)

	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: gosync [options] <source> <destination>\n\n")
		fmt.Fprintf(os.Stderr, "Adaptive parallel file transfer tool\n\n")
		fmt.Fprintf(os.Stderr, "Subcommands:\n")
		fmt.Fprintf(os.Stderr, "  serve     Run as a server listening for incoming transfers\n\n")
		fmt.Fprintf(os.Stderr, "Options:\n")
		flag.PrintDefaults()
		fmt.Fprintf(os.Stderr, "\nExamples:\n")
		fmt.Fprintf(os.Stderr, "  gosync -transport quic -workers 8 /path/to/source user@host:/path/to/dest\n")
		fmt.Fprintf(os.Stderr, "  gosync -transport ssh -resume -checksum /music/ user@server:/mnt/music/\n")
		fmt.Fprintf(os.Stderr, "  gosync -dry-run -verbose /data/ user@host:/backup/\n")
		fmt.Fprintf(os.Stderr, "  gosync -exclude '*.tmp,*.log' /src/ user@host:/dst/\n")
		fmt.Fprintf(os.Stderr, "  gosync -deploy -transport ssh user@host:/data/\n")
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

	source := flag.Arg(0)
	destination := flag.Arg(1)

	if *deployFlag {
		runDeploy(source, destination, *deployKey, *deployListen, *transportType, *workers, *bandwidth, *compression, *dryRun, *verbose, *quiet, *resume, *checksum)
		return
	}

	if _, err := os.Stat(source); os.IsNotExist(err) {
		fmt.Fprintf(os.Stderr, "Error: source path does not exist: %s\n", source)
		os.Exit(1)
	}

	var transportImpl transport.Transport
	config := transport.Config{
		Host:        extractHost(destination),
		Port:        extractPort(destination),
		Username:    extractUser(destination),
		Timeout:     30,
		MaxRetries:  3,
		Compression: *compression,
		Bandwidth:   *bandwidth,
	}

	switch *transportType {
	case "tcp":
		transportImpl = transport.NewTCPTransport(config)
	case "quic":
		transportImpl = transport.NewQUICTransport(config)
	case "ssh":
		transportImpl = transport.NewSSHTransport(config)
	case "server":
		transportImpl = transport.NewServerTransport(config)
	default:
		fmt.Fprintf(os.Stderr, "Error: unsupported transport: %s\n", *transportType)
		os.Exit(1)
	}

	var excludeList, includeList []string
	if *excludes != "" {
		excludeList = strings.Split(*excludes, ",")
	}
	if *includes != "" {
		includeList = strings.Split(*includes, ",")
	}

	syncConfig := sync.Config{
		Workers:     *workers,
		Bandwidth:   *bandwidth,
		Compression: *compression,
		DryRun:      *dryRun,
		Verbose:     *verbose,
		Quiet:       *quiet,
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

func runDeploy(source, destination, keyFile, listenAddr, transportType string, workers int, bandwidth int64, compression, dryRun, verbose, quiet, resume, checksum bool) {
	d := deploy.NewDeployer(source, destination, keyFile)
	if err := d.Deploy(); err != nil {
		fmt.Fprintf(os.Stderr, "Deploy failed: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("Deployed gosync to %s\n", d.GetHost())
	fmt.Printf("Remote server listening on %s:8443\n", d.GetHost())

	config := transport.Config{
		Host:        d.GetHost(),
		Port:        8443,
		Username:    d.GetUsername(),
		Timeout:     30,
		MaxRetries:  3,
		Compression: compression,
		Bandwidth:   bandwidth,
	}

	transportImpl := transport.NewServerTransport(config)

	var excludeList, includeList []string
	syncConfig := sync.Config{
		Workers:     workers,
		Bandwidth:   bandwidth,
		Compression: compression,
		DryRun:      dryRun,
		Verbose:     verbose,
		Quiet:       quiet,
		Resume:      resume,
		Checksum:    checksum,
		Excludes:    excludeList,
		Includes:    includeList,
	}

	deployDest := fmt.Sprintf("%s@%s:8443", d.GetUsername(), d.GetHost())
	s := sync.NewSync(source, deployDest, transportImpl, syncConfig)
	if err := s.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "Sync failed: %v\n", err)
		os.Exit(1)
	}
}

func runServe() {
	serveCmd := flag.NewFlagSet("serve", flag.ExitOnError)
	listenAddr := serveCmd.String("listen", "0.0.0.0:8443", "Listen address")
	baseDir := serveCmd.String("base", "/tmp/gosync-receive", "Base directory for received files")

	serveCmd.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: gosync serve [options]\n\n")
		fmt.Fprintf(os.Stderr, "Run as a server listening for incoming transfers\n\n")
		fmt.Fprintf(os.Stderr, "Options:\n")
		serveCmd.PrintDefaults()
	}

	serveCmd.Parse(os.Args[2:])

	srv := server.NewServer(*listenAddr, *baseDir)
	if err := srv.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "Server error: %v\n", err)
		os.Exit(1)
	}
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
