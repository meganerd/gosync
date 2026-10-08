# Gosync

Adaptive parallel file transfer tool with selectable transport backends (QUIC, TCP, SSH).

Use `--deploy` to transfer without manually installing or starting gosync on the
destination host:

```bash
gosync --deploy /local/source user@remote:/destination
```

The flag uses SSH to copy, start, and clean up a temporary remote gosync
receiver. File data then uses the selected `--transport` (`quic` by default,
or `tcp`/`server`); it does not travel through the SSH connection.

## Features

- **Adaptive Parallelism**: Automatically scales worker count based on available resources
- **Multiple Transport Backends**: Choose between QUIC (UDP), TCP, or SSH
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

# Application SHA-256 on (compatible with pre-CAPS size-first/base64 peers)
gosync --checksum=true source destination

# Application SHA-256 off at both endpoints (default; updated receiver required)
gosync --checksum=false source destination

# Dry run (show what would be transferred)
gosync --dry-run source destination

# Verbose output
gosync --verbose source destination

# Exclude patterns
gosync --exclude "*.log" --exclude ".git" source destination

# Include patterns (only transfer matching files)
gosync --include "*.mp3" --include "*.flac" source destination
```

### Checksum mode and migration

`-checksum` defaults to **false**. For `quic`, `tcp`, and `server`, including
`-deploy`, false disables application SHA-256 on **both endpoints**; size framing
and acknowledgements remain. During `Connect`, a separate `CAPS` exchange must
return exactly `OK CAPS NOHASH` before the client uses `SEND-NOHASH` or
`RECEIVE-NOHASH`. Unsupported receivers fail **before file data is sent**. Upgrade
the receiver, or use `-checksum` (`-checksum=true`) only if it already supports the
pre-CAPS size-first/base64 SHA-256 protocol. There is **no automatic fallback**.

True retains the immediate pre-CAPS `SEND <size> <base64path>` / `RECEIVE`
SHA-256 protocol with strict digest and size checking in the acknowledgement,
framing, and trailer. Compatibility is limited to peers using that protocol,
not arbitrary older versions or the historical HEAD receiver, which used
`SEND <literal-path> <size>`. For that older path-first protocol, **upgrade both
ends**; no protocol auto-detection or compatibility is promised. Size-first/base64
framing was already present before the checksum-mode change, not introduced by it. `gosync serve` selects the mode per request; no server-global
checksum flag is needed, and the deployment client dictates the mode.

```sh
# QUIC deployment: explicitly disable or enable application hashing.
gosync -deploy -checksum=false /local/source user@remote:/destination
gosync -deploy -checksum=true /local/source user@remote:/destination
```

QUIC TLS encryption, certificate verification, and leaf pinning are unchanged in
**either mode**. TCP/server remains unencrypted and, with false, has no application
digest. Standalone `--transport ssh --checksum=true` is now rejected (previously
silently ignored); omit `--checksum` for SSH. SSH transport integrity is separate
from gosync's application SHA-256.

**Historical benchmark caveat:** before this change, these gosync network paths
always hashed, even with `-checksum=false`. Existing results are not measurements
of the new no-hash mode and imply no performance gain from it.

### Transfer Progress

Overall transfer progress is enabled by default. Use `-quiet` (or `--quiet`)
to suppress progress and other non-error transfer output:

```bash
gosync source destination
gosync -quiet source destination
gosync --deploy source user@host:/backup
gosync source destination 2>progress.log
```

Progress reports overall transferred bytes and total bytes, percentage, average
transfer rate, estimated time remaining (ETA), and completed and failed file
counts. It is written to **stderr**, using a live indicator in a terminal and
newline-separated snapshots when stderr is redirected. `--quiet` overrides
progress, and `--dry-run` does not display it. The `--progress`/`-P` flags remain
available; `--progress=false` hides only progress while retaining the summary.

The progress display and final summary repeat the absolute local source and
remote destination (including host and port). Updated TCP/server and QUIC receivers report
their actual absolute base directory, including in deployment mode. When that
information is unavailable, the destination is explicitly labeled as remote-relative
rather than guessed. This describes the existing transfer paths; it does not
change where files are stored.

`-P` is only an alias for `--progress`. Unlike rsync's `-P`, it does **not** enable
partial-file retention or resume.

### Server Mode

```bash
# Start a TCP transfer server (serve defaults to tcp)
gosync serve --listen :8443

# Transfer to an existing TCP server
gosync --transport tcp /local/source user@remote:8443:/destination

# Deploy a server via SSH, then transfer via QUIC (the transfer default)
gosync --deploy /local/source user@remote:/destination
```

### Transport Selection

`--transport` selects the protocol for **actual file transfer**. Transfer commands
default to **quic**, including with `--deploy`; `gosync serve` defaults to **tcp**.
Match the client and receiver protocols when starting a server manually.

`--deploy` uses SSH to copy/start (and clean up) the remote binary, then honors
`--transport quic`, `tcp`, or `server` for file data (`server` uses TCP).
`--deploy --transport ssh` and unknown transports are rejected; there is no
silent TCP override. SSH handles setup and certificate retrieval; actual QUIC file
data travels directly over UDP, authenticated with the retrieved TLS leaf pin,
not through an SSH tunnel.

On Linux, a block device can be used as a single-file source. gosync reads its
capacity with `BLKGETSIZE64`; the invoking user must have permission to open the
device. In deployment mode the destination remains a receiver directory, so
`gosync --deploy /dev/nvme2n1 host:/recovery/image` writes
`/recovery/image/nvme2n1`. Rename that completed file if a specific image name
is required. Pipes and character devices are rejected because they do not have
a finite size for the transfer protocol.

QUIC fan-out writes to a sibling `*.gosync-<token>.part` file and atomically
renames it only after the receiver has flushed it successfully. Finalization of
large images can take substantially longer than ordinary network operations;
gosync scales the commit wait with image size. A `.part` left after an
interrupted deployment is not automatically resumable because range state is
held by the receiver process. If the sender reported all bytes transferred and
the staging file has the exact source size, an operator can flush and rename it
in place after performing whatever integrity verification the workload needs.

`gosync serve --transport quic` runs the production QUIC receiver over UDP;
`--transport tcp` selects TCP. The CLI reports the selected file-transfer transport
and network protocol (unless quiet), separately identifying SSH deployment.
These are selection messages, not proof of a successful connection. Server startup
logs report the actual bound address and network, including UDP for QUIC.

**QUIC security:** TLS authenticates the receiver and uses ALPN `gosync` (not
HTTP/3). On a transfer command, `-cert` names a **local public certificate** used
as an **exact leaf pin**, not a CA bundle. Without `-cert`, a non-deployment
transfer uses system PKI trust and hostname checking.

QUIC `-deploy` automatically fetches the receiver's public ephemeral certificate
over SSH with `StrictHostKeyChecking=yes` and pins it for the transfer. Verify the
SSH host key and provision `known_hosts` **before deployment**; unknown or changed
host keys are rejected. Private TLS keys remain on the receiver.

To deploy with existing remote TLS files, supply `-remote-cert` and `-remote-key`
together. These options are valid only with QUIC `-deploy`. An optional local
`-cert` remains the expected leaf pin; `-deploy -cert` without the remote pair is
rejected. For a manual receiver, `serve -cert` and `-key` must be supplied together;
without them it generates an ephemeral identity in memory. `serve -cert-out`
exports only the public leaf to an exclusively created file (an existing path is
not overwritten). All TLS flags are invalid with non-QUIC transports.

**There is no client authentication.** Restrict receiver network access to
intended senders, even with authenticated TLS. SSH setup does not authorize clients
connecting to the receiver's UDP port. See [QUIC TLS configuration](docs/configuration.md#quic-security-and-compatibility)
for trust setup and additional examples.

QUIC shares the server's base64-encoded paths and size framing, with checksum
behavior selected as described above. Its wire format differs from the old
experimental QUIC prototype: **update both client and receiver binaries**.
Pre-CAPS size-first/base64 TCP SHA-256 peers remain compatible with
`-checksum=true`; older path-first peers require both ends to be upgraded.
The default no-hash mode requires receiver support for the new commands.

```bash
# Use TCP with a TCP receiver
gosync --transport tcp source destination

# On the receiver: export its ephemeral public leaf to a new file.
gosync serve --transport quic --listen :8443 --base /destination --cert-out /secure/receiver-leaf.pem

# On the sender: first obtain that public file over a trusted channel.
gosync --transport quic --cert ./receiver-leaf.pem /local/source user@remote:8443:/destination

# Auto-fetch and pin via SSH; verify/provision the host key in known_hosts first.
gosync --deploy /local/source user@remote:/destination

# Existing TLS files on the receiver; the private key is never downloaded.
gosync --deploy --remote-cert /etc/gosync/server.pem --remote-key /etc/gosync/server.key --cert ./expected-leaf.pem /local/source user@remote:/destination

# Explicit TCP deployment; omitting --transport defaults to QUIC
gosync --deploy --transport tcp /local/source user@remote:/destination

# Use SSH
gosync --transport ssh source destination
```

## Configuration

Gosync can be configured via JSON file or command-line flags:

```json
{
  "workers": 8,
  "transport": "quic",
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

### Asynchronous SHA-256

With `-checksum=true`, TCP/server and QUIC transfers (including `--deploy`) overlap file/socket I/O with an
ordered SHA-256 worker on each endpoint. Each active copy uses four reusable
payload buffers (32 KiB each by default, 128 KiB total), with backpressure when hashing falls
behind. Success still waits for the final digest and checksum verification;
the checksum algorithm is unchanged. Both endpoints must run the updated binary
to pipeline both sides. Only pre-CAPS size-first/base64 TCP SHA-256 peers remain
wire-compatible in this hash-enabled mode; older path-first peers and the old
experimental QUIC wire format are not compatible. With the default
`-checksum=false`, neither endpoint uses the hashing worker; each copy uses one buffer.

This overlaps hashing with I/O; it does not parallelize a file's SHA-256 digest
or remove its CPU throughput limit. The buffer-comparison results below are TCP-only.
A full-ISO QUIC deployment to Agrippa has also been validated with matching SHA-256
and captured UDP traffic; see [validation details](docs/benchmark-results.md#production-quic-deployment-validation).

### Transfer buffer size

`--buffer-size` sets **bytes per copy buffer** for TCP/server and QUIC transfers. The
default remains **32768** (32 KiB); accepted values are **4096–4194304** (4 KiB–4 MiB).
With `-checksum=true`, four buffers per active copy mean payload memory is four
times this setting on each endpoint: 1 MiB at 256 KiB buffers, or 4 MiB at 1 MiB
buffers. With `-checksum=false`, each active copy uses one buffer: 32 KiB by
default, 256 KiB at a 256 KiB setting, or 1 MiB at a 1 MiB setting.

```sh
# Deployment: the setting is passed to BOTH the sender and deployed receiver.
./gosync -deploy -buffer-size 262144 -checksum /path/to/file.iso host:/destination/

# Manual server: configure each endpoint explicitly (no size negotiation).
./gosync serve -base /destination -listen :9444 -buffer-size 262144
./gosync -transport tcp -buffer-size 262144 /path/to/file.iso host:9444:/file.iso
```

Different endpoint sizes remain compatible. Explicit buffer settings with the
standalone SSH transport are rejected rather than silently ignored.

### QUIC connection fan-out

`-connections N` sets how many **QUIC data connections (sender sockets)** carry a
single file, from `1` to `16`. QUIC defaults to **4** so large single-file
transfers use fan-out without an opt-in flag. Other transports remain at one;
pass `-connections 1` to reproduce the legacy QUIC wire behavior exactly.

```sh
# Four sender sockets per file; the receiver keeps one port.
./gosync -transport quic -connections 4 /path/to/big.iso host:9444:/big.iso

# Deployment passes the count to the receiver so it sizes its socket group.
./gosync -deploy -connections 4 /path/to/big.iso host:/destination/
```

What it applies to and when it engages:

- **QUIC data transfer only.** With `tcp`, `server` or `ssh`, a value above 1 is
  rejected rather than silently ignored.
- **Files below 64 MiB use one connection** regardless of the flag: setup cost
  dominates below that threshold.
- The receiver advertises its own maximum, and `min(requested, advertised)` is
  used. A receiver without fan-out support answers the capability query with an
  error and the transfer proceeds over one connection, with no retry loop.
- `-connections` multiplies sockets per file, `-workers` multiplies files in
  flight; the total number of data sockets is capped at 16.
- Every data connection uses the same TLS verification and certificate pinning
  as the control connection.

With `-checksum`, ranged transfers still exchange a SHA-256 digest per range,
and the commit carries the whole-file digest: **the receiver reads the finished
file back from the destination and hashes it before renaming it into place.**
That costs a full destination read plus receiver-side hashing, which is
significant on slow storage and on hosts without SHA-NI; the sender likewise
re-reads the source to compute its whole-file digest. Checksum-off transfers
pay none of this.

The measurement that motivates fan-out is in
[the design](docs/multi-connection-quic-design.md): sender socket count carried
QUIC scaling in a standalone harness (901 → 2179 MiB/s at eight sockets), while
extra streams or extra connections on one socket did not. Those are harness
numbers without disk I/O or framing, so no fixed end-to-end speed is promised.
Four connections captured 92% of the eight-connection gain and is the QUIC
default.

Historically, with application SHA-256 active on both endpoints, three rotated-order
**TCP** full-ISO trials per size on Agrippa's RAM disk measured
median **268.78 MB/s (32 KiB)**, **287.42 MB/s (256 KiB)**, and **289.87 MB/s (1 MiB)**.
Larger buffers gave a repeatable ~7–8% benefit on this workload, but this is not a
universal guarantee. 256 KiB achieved nearly the same median as 1 MiB with a
quarter of its buffer memory; the conservative default is unchanged.

### Repeatable benchmarks

- [TCP versus rsync/SSH runner](docs/benchmark-transfer.md): alternating full
  transfers with SHA-256 verification, isolated receiver, and saved JSON results.
- [Buffer/checksum microbenchmarks and CPU profiles](docs/benchmark-internals.md).
- [Measured Agrippa RAM-disk results](docs/benchmark-results.md).

Benchmark artifacts are saved under `benchmark-results/` and ignored by Git.

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
