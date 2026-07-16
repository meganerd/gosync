# Gosync

Adaptive parallel file transfer tool with selectable transport backends (QUIC/HTTP3, TCP, SSH).

## Features

- **Adaptive Parallelism**: Automatically scales worker count based on available resources
- **Multiple Transport Backends**: Choose between QUIC/HTTP3 (UDP), TCP, or SSH
- **High Performance**: Optimized for high-speed local networks (40 Gbps+ Ethernet, 565 Gbps+ Infiniband)
- **Cross-Platform**: Works on Linux, macOS, and Windows (AMD64 and ARM64)
- **Resume Support**: Automatic resume of interrupted transfers
- **Compression**: Optional gzip compression
- **Progress Tracking**: Real-time transfer progress and statistics

## Quick Start

### Linux/macOS

```bash
# Build from source
go build -o gosync ./cmd/gosync

# Or download pre-built binary
# See releases page for your platform

# Basic usage
gosync /path/to/source /path/to/destination

# With options
gosync --workers 8 --progress --checksum /source /destination
```

### Windows

See [Windows Documentation](docs/README.windows.md) for detailed installation and usage instructions.

```cmd
# Basic usage
gosync C:\source D:\destination

# With options
gosync --workers 8 --progress --checksum C:\source D:\destination
```

## Installation

### Pre-built Binaries

Download the latest release for your platform from the [Releases page](https://gitlab.zarquon.space/meganerd/gosync/-/releases).

Available binaries:
- Linux: `gosync-linux-amd64`, `gosync-linux-arm64`
- macOS: `gosync-darwin-amd64`, `gosync-darwin-arm64`
- Windows: `gosync-windows-amd64.exe`, `gosync-windows-arm64.exe`

### Build from Source

```bash
# Clone the repository
git clone https://gitlab.zarquon.space/meganerd/gosync.git
cd gosync

# Build
make build

# Or build for specific platform
make dist-linux    # Linux (amd64 + arm64)
make dist-darwin   # macOS (amd64 + arm64)
make dist-windows  # Windows (amd64 + arm64)
```

## Usage

### Basic Transfer

```bash
# Transfer a single file
gosync source.txt destination.txt

# Transfer a directory
gosync /path/to/source /path/to/destination
```

### Options

```bash
# Parallel workers
gosync --workers 8 source destination

# Progress tracking
gosync --progress source destination

# Checksum verification
gosync --checksum source destination

# Dry run (show what would be transferred)
gosync --dry-run source destination

# Verbose output
gosync --verbose source destination

# Exclude patterns
gosync --exclude "*.log" --exclude ".git" source destination

# Include patterns (only transfer matching files)
gosync --include "*.mp3" --include "*.flac" source destination
```

### Server Mode

```bash
# Start a transfer server
gosync serve --listen :8443

# Transfer to a server
gosync --deploy user@remote:/path/to/source /local/destination
```

### Transport Selection

```bash
# Use TCP (default)
gosync --transport tcp source destination

# Use QUIC/HTTP3 (UDP)
gosync --transport quic source destination

# Use SSH
gosync --transport ssh source destination
```

## Configuration

Gosync can be configured via JSON file or command-line flags:

```json
{
  "workers": 8,
  "transport": "tcp",
  "checksum": true,
  "compression": false,
  "progress": true,
  "verbose": false
}
```

See [Configuration Documentation](docs/configuration.md) for details.

## Performance Tuning

For high-speed networks (40 Gbps+), consider:

1. Increase worker count: `--workers 16`
2. Disable compression if data is already compressed
3. Ensure adequate buffer sizes
4. Tune OS network settings (see [Performance Guide](docs/performance.md))

## Development

### Prerequisites

- Go 1.22 or later
- Make (optional, for build targets)

### Building

```bash
# Build for current platform
make build

# Run tests
make test

# Build for all platforms
make dist
```

### Testing

```bash
# Run all tests
make test

# Run tests with coverage
make test-cover

# Run specific package tests
go test ./pkg/scanner/...
```

## License

[License TBD]

## Support

- [GitLab Issues](https://gitlab.zarquon.space/meganerd/gosync/-/issues)
- [Documentation](docs/)
