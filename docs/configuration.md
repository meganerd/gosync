# Configuration

Gosync can be configured through command-line flags, environment variables, or a JSON configuration file.

## Configuration File

Create a `config.json` file in your current directory or specify a custom path:

```bash
gosync --config /path/to/config.json source destination
```

### Example Configuration

```json
{
  "workers": 8,
  "transport": "tcp",
  "checksum": true,
  "compression": false,
  "progress": true,
  "verbose": false,
  "dry_run": false,
  "exclude": ["*.log", ".git", "__pycache__"],
  "include": ["*.mp3", "*.flac"],
  "server": {
    "listen": ":8443",
    "api_key_env": "GOSYNC_API_KEY"
  },
  "deploy": {
    "enabled": false,
    "target": "",
    "key": ""
  }
}
```

## Configuration Options

### Transfer Options

| Option | Type | Default | Description |
|--------|------|---------|-------------|
| `workers` | int | Auto-detected | Number of parallel transfer workers |
| `transport` | string | "quic" | File-transfer backend: "quic", "tcp", "ssh", "server"; not the SSH deployment protocol |
| `checksum` | bool | false | Application SHA-256 at both QUIC/TCP/server endpoints; false disables hashing, true enables strict digest/size verification; true is rejected for SSH |
| `compression` | bool | false | Enable gzip compression |
| `progress` | bool | true | Show transfer progress; suppressed by `--quiet` or `--dry-run` |
| `verbose` | bool | false | Enable verbose logging |
| `dry_run` | bool | false | Show what would be transferred without transferring |

### Filter Options

| Option | Type | Default | Description |
|--------|------|---------|-------------|
| `exclude` | []string | [] | Patterns to exclude (glob syntax) |
| `include` | []string | [] | Patterns to include (glob syntax) |

### Server Options

| Option | Type | Default | Description |
|--------|------|---------|-------------|
| `server.listen` | string | ":8443" | Address and port to listen on |
| `server.api_key_env` | string | "GOSYNC_API_KEY" | Environment variable for API key |

### Deploy Options

| Option | Type | Default | Description |
|--------|------|---------|-------------|
| `deploy.enabled` | bool | false | Enable deployment mode |
| `deploy.target` | string | "" | Target host for deployment |
| `deploy.key` | string | "" | SSH key for deployment |

## Command-Line Flags

All configuration options can be set via command-line flags:

```bash
gosync --workers 8 --transport quic --checksum --progress source destination
```

### Transport selection and deployment

Transfer commands default to `--transport quic`, including with `--deploy`.
`gosync serve` defaults to `--transport tcp` and accepts `tcp` or `quic`.
For library callers, `server.SetTransport("tcp")` or `server.SetTransport("quic")`
selects the production receiver protocol before `Start`; other values are rejected.

`--deploy` always uses SSH for remote setup and cleanup, but honors `quic`, `tcp`,
or `server` for actual file data. `server` selects the TCP server backend and
starts a TCP receiver. `--deploy --transport ssh` and unknown transports are
rejected, not overridden. The selected transport is reported at runtime, separately
from SSH bootstrap; server startup logs show the actual bound address and network
(UDP for QUIC). Selection output alone does not establish connection success.

```sh
# Manual QUIC receiver: export the public leaf to a new file.
gosync serve --transport quic --listen :9444 --base /destination --cert-out /secure/receiver-leaf.pem
# Sender: obtain the public leaf over a trusted channel before transferring.
gosync --transport quic --cert ./receiver-leaf.pem source host:9444:/payload

# SSH bootstrap and automatic leaf pinning, QUIC file transfer (default).
# Verify the SSH host key and provision known_hosts first.
gosync --deploy source host:/destination/

# SSH bootstrap, explicit TCP file transfer.
gosync --deploy --transport tcp source host:/destination/
```

A manually started receiver must match the client's protocol: use `--transport tcp`
or `server` with a default `gosync serve`, or start `serve --transport quic` for
QUIC. SSH setup does not tunnel the file data; the selected TCP/UDP port must be
reachable directly.

### Application checksum mode and compatibility

`-checksum=false` is the CLI default for `quic`, `tcp`, and `server`, including
`-deploy`. It disables application SHA-256 computation and digest verification
on **both endpoints**, not merely a final comparison. `-checksum` and
`-checksum=true` enable it.

| Mode | Connection and transfer protocol | Completion checks |
|------|----------------------------------|-------------------|
| `false` (default) | During `Connect`, a separate `CAPS\n` request must receive exactly `OK CAPS NOHASH\n`; then use `SEND-NOHASH` / `RECEIVE-NOHASH` | Exact-length payloads; send ACK `OK NONE <size>`; receive header `OK SIZE <size>` and trailer `END <size>`; sizes strictly checked, no digest |
| `true` | Pre-CAPS `SEND <size> <base64path>` / `RECEIVE` SHA-256 protocol; no NOHASH capability requirement | SHA-256 computed at both endpoints; send ACK `OK <digest> <size>`; receive header `OK SIZE <size>` and trailer `CHECKSUM <digest>`; strict digest and size/framing validation |

The capability exchange is separate from the existing connection probe (a
separate stream for QUIC). Missing, malformed, or unsupported capability replies
fail `Connect` before file data is sent. There is **no fallback** to hashing or to
an unnegotiated no-hash transfer. Upgrade the receiver, or explicitly use
`-checksum` only with peers already supporting the immediate pre-CAPS
size-first/base64 SHA-256 protocol (`SEND <size> <base64path>` / `RECEIVE`).

**Compatibility boundary:** this does not cover arbitrary older versions or the
historical HEAD receiver's path-first `SEND <literal-path> <size>` protocol.
For those older peers, **upgrade both client and receiver**; no protocol
auto-detection or compatibility is promised. Size-first/base64 framing was already
in the working tree before the checksum-mode change; it was not introduced by
that change. `-checksum` also does not restore compatibility with the old
experimental QUIC prototype.

`gosync serve` chooses hashing **per request command**, so it needs no global
checksum flag and can serve both modes. With `-deploy`, the client dictates the
mode; SSH bootstrap does not change the checksum choice.

```sh
# One manually started TCP receiver supports either request mode.
gosync serve -transport tcp -listen :9444 -base /destination
# Client: off requires NOHASH support; on also supports pre-CAPS size-first/base64 SHA-256 peers.
gosync -transport tcp -checksum=false source host:9444:/payload
gosync -transport tcp -checksum=true source host:9444:/payload

# QUIC deployment; TLS trust/pinning is identical in either mode.
gosync -deploy -checksum=false source host:/destination/
gosync -deploy -checksum=true source host:/destination/

# Standalone SSH: do not request gosync application checksums.
gosync -transport ssh source host:/destination/
```

**Security:** QUIC TLS encryption, certificate verification, and leaf pinning
remain enabled and unchanged in either mode. TCP/server remains unencrypted;
false also removes its application digest. Size checks are not cryptographic
integrity checks. Standalone SSH retains its own transport integrity, which is
separate from application SHA-256. `-transport ssh -checksum=true` is now rejected
rather than silently ignored; remove `-checksum` from existing SSH commands.

**Library callers:** `transport.Config.Checksum` has a zero-value default of
false. Set it to true for pre-CAPS size-first/base64 hash-enabled behavior when using a transport
directly. `sync.Config.Checksum` is propagated to checksum-configurable transports
before `Connect`; set it explicitly when using the sync layer. Changing a
transport's checksum mode requires reconnecting for capability negotiation.

**Migration:** earlier QUIC/TCP/server implementations always hashed regardless
of the false flag. The default now genuinely turns application hashing off.
Upgrade receivers before relying on the default. Explicit true is a compatibility
option only for pre-CAPS size-first/base64 SHA-256 peers; older path-first peers
must upgrade both ends. Historical timings must not be relabeled as no-hash
results; new measurements are needed to assess this change's performance.

### QUIC security and compatibility

QUIC TLS authenticates the receiver and encrypts the transfer, using ALPN `gosync`
(not HTTP/3). The trust mode depends on whether the receiver is deployed via SSH:

- **Manual transfer with `-cert`:** the path is a local PEM public certificate
  used as an **exact leaf pin**, not a CA bundle. The receiver must present that
  leaf; trusting its issuer or another certificate with the same hostname is not
  sufficient. Pinning replaces system-PKI/hostname-based identity verification.
- **Manual transfer without `-cert`:** normal system PKI trust and hostname
  checking apply. The receiver needs a certificate trusted by the sender's system
  and valid for the destination hostname. An automatically generated self-signed
  certificate is not trusted by default.
- **QUIC `-deploy`:** by default the receiver generates an ephemeral certificate
  and private key in memory. Deployment retrieves only the public certificate
  over host-key-verified SSH and pins that leaf for the QUIC connection. SSH uses
  `StrictHostKeyChecking=yes`; verify the host's key through a trusted channel
  and provision the sender's SSH `known_hosts` before deployment. Unknown or
  changed host keys are rejected, not automatically accepted.

#### TLS flags and file locations

Single- and double-dash forms are accepted in these examples.

| Command | Flags | Meaning and constraints |
|---------|-------|-------------------------|
| Transfer | `-cert LOCAL.pem` | Local public exact leaf pin; not a receiver certificate path or a private key |
| QUIC `-deploy` | `-remote-cert REMOTE.pem -remote-key REMOTE.key` | Required pair selecting existing TLS files on the remote receiver; invalid without `-deploy` |
| QUIC `-deploy` with remote pair | `-cert LOCAL.pem` (optional) | Local certificate remains the expected leaf pin; it is not replaced by a different fetched certificate |
| QUIC `serve` | `-cert RECEIVER.pem -key RECEIVER.key` | Required pair selecting the receiver's TLS identity; omit both to generate an ephemeral identity in memory |
| QUIC `serve` | `-cert-out NEW.pem` | Export only the public leaf certificate, for either generated or supplied identity; creates the file exclusively and refuses an existing path |

`-deploy -cert` without the `-remote-cert`/`-remote-key` pair is rejected.
Supplying only one member of either certificate/key pair is also rejected.
All TLS flags are invalid with non-QUIC transports (`tcp`, `server`, or `ssh`).
Private TLS keys remain on the receiver: neither public-leaf export nor SSH
certificate retrieval downloads them. A generated identity changes when the
receiver restarts, so manual clients must securely obtain its new public pin;
use a fresh `-cert-out` path rather than an existing file.

#### Examples

For a manual receiver with existing TLS files, start it on the receiver host:

```sh
gosync serve -transport quic -listen :9444 -base /destination -cert /etc/gosync/server.pem -key /etc/gosync/server.key -cert-out /secure/new-leaf.pem
```

On the sender, obtain the exported **public leaf** through a trusted channel
(such as SSH with a previously verified host key), then pin it:

```sh
scp -o StrictHostKeyChecking=yes user@host:/secure/new-leaf.pem ./receiver-leaf.pem
gosync -transport quic -cert ./receiver-leaf.pem source host:9444:/payload

# Alternatively, no pin: requires system-trusted PKI and a matching hostname.
gosync -transport quic source receiver.example.com:9444:/payload
```

Deployment examples below require the SSH host key to be verified and present in
`known_hosts` beforehand. Actual file data uses QUIC over UDP, not SSH:

```sh
# Generate an ephemeral receiver identity, fetch its public leaf, and pin it.
gosync -deploy source user@host:/destination/

# Select existing remote TLS files and automatically pin the fetched public leaf.
gosync -deploy -remote-cert /etc/gosync/server.pem -remote-key /etc/gosync/server.key source user@host:/destination/

# Also require the receiver to match a separately obtained local public leaf.
gosync -deploy -remote-cert /etc/gosync/server.pem -remote-key /etc/gosync/server.key -cert ./expected-leaf.pem source user@host:/destination/
```

**No client authentication is provided.** Server authentication does not authorize
senders: restrict receiver network access to intended clients, for example with
firewall rules and an appropriately scoped listen address. Do not expose the
receiver to untrusted clients. SSH authenticates setup and public-certificate
retrieval; QUIC TLS authenticates the receiver during the actual UDP data transfer.
The UDP port must be reachable directly and is not protected by SSH login controls.
SHA-256 checksums provide integrity checking, not client authentication.

QUIC uses the shared protocol with base64-encoded paths and exact-length payloads;
application digest verification depends on the checksum mode above. The QUIC
client wire format changed from the old experimental prototype; update **both
binaries**. Pre-CAPS size-first/base64 TCP SHA-256 framing remains compatible with
`-checksum=true`; older path-first peers require both ends to be upgraded, and
no-hash commands require an updated receiver. A historical full-ISO remote deployment
with application hashing active was validated with matching SHA-256 and UDP
packet capture; this is not evidence of a general QUIC performance advantage.

### TCP/server/QUIC buffer size

`--buffer-size BYTES` sets bytes per copy buffer (default `32768`, range `4096`
through `4194304`) on transfer commands and `gosync serve`. TCP/server and QUIC
allocate **one buffer** per active copy with `-checksum=false`: 32 KiB by default,
4 MiB at the maximum. With `-checksum=true`, the asynchronous SHA-256 pipeline
uses **four buffers** per active copy: 128 KiB by default, 16 MiB at the maximum;
completion waits for hashing and checksum verification. These are payload-buffer
budgets per endpoint, before other allocations; memory scales with concurrency.

`--deploy` forwards the buffer size to the remote receiver; otherwise configure
sender and receiver separately. Buffer sizes need no negotiation and endpoints
can use different sizes (the no-hash capability check is separate). This option
does not apply to standalone SSH; explicit use with SSH is rejected.

```sh
gosync --deploy --buffer-size 262144 source host:/destination/
```

### QUIC data connections (fan-out)

`--connections N` sets the number of QUIC **data connections (sender sockets)**
used per file (QUIC default `4`, range `1` through `16`). Each connection is an
independent UDP socket with its own QUIC connection, dialing the single port the
receiver advertises, and carrying one contiguous byte range of the file.

- **Explicit `--connections 1` reproduces legacy behavior exactly.** It sends the existing
  `SEND`/`SEND-NOHASH` commands, asks the receiver nothing new, and opens no
  additional sockets. TCP, SSH, and server transports implicitly remain at one.
- **QUIC data transfer only.** A value above 1 with `--transport tcp`, `server`
  or `ssh` is rejected rather than silently ignored, the same way
  `--transport ssh --checksum=true` is rejected.
- **Files below 64 MiB always use one connection**, whatever the flag says,
  because fan-out cannot repay its setup cost at that size.
- The sender asks the receiver once, right after the existing `PING`/`CAPS`
  handshake, and uses `min(requested, receiver maximum)` sockets. A receiver
  without fan-out support replies with its unknown-command error and the
  transfer silently proceeds over one connection; there is no retry loop.
- Sources that cannot be read at an offset (pipes and other stream-only
  readers) use one connection, because ranges are read concurrently and the
  source is never seeked.
- `--connections` multiplies sockets per file while `--workers` multiplies files
  in flight. Total concurrent data sockets are capped at 16, and per-file
  fan-out is reduced to respect that cap.
- Every data connection performs the same TLS verification and certificate
  pinning as the control connection.
- `--deploy` passes the count to the deployed QUIC receiver so it sizes its
  `SO_REUSEPORT` socket group. The deployment port probe is unchanged: still a
  single UDP port, so the firewall surface does not grow.
- If any range cannot be completed within the retry budget, the sender aborts
  the transfer and **no partial file appears at the destination path**. A range
  that fails is retried at its original offset on another connection.

```sh
gosync --transport quic --connections 4 /path/to/big.iso host:9444:/big.iso
gosync --deploy --connections 4 /path/to/big.iso host:/destination/
```

#### Cost of `--checksum` with fan-out

SHA-256 of a whole file cannot be composed from per-range digests, so ranged
transfers with `--checksum` verify at commit time:

- Each range is still verified in flight by its own digest, so a corrupt range
  is detected during transfer rather than only at the end.
- At commit the sender sends the SHA-256 of the whole source file, and **the
  receiver reads the entire staged file back from the destination and hashes it**
  before renaming it into place. Commit time therefore grows with file size and
  is bounded by destination read throughput: cheap on a RAM disk, a significant
  share of total time on slow storage.
- That hashing happens on the receiver's CPU, which matters on hosts without
  SHA-NI, where hashing has historically been the bottleneck.
- The sender also re-reads its source to compute the whole-file digest, so the
  source is read twice (usually from page cache).
- A mismatch aborts the commit and removes the staged file, so a file that fails
  verification never appears at the destination path.
- With `--checksum=false` (the default) neither endpoint reads the file back and
  none of this cost applies.

### Flag Priority

Command-line flags override configuration file values, which override default values.

## Environment Variables

| Variable | Description |
|----------|-------------|
| `GOSYNC_API_KEY` | API key for authenticated transfers |
| `GOSYNC_WORKERS` | Number of parallel workers |
| `GOSYNC_TRANSPORT` | Transport backend |
| `GOSYNC_CHECKSUM` | Enable checksum verification |
| `GOSYNC_COMPRESSION` | Enable compression |
| `GOSYNC_PROGRESS` | Show progress |
| `GOSYNC_VERBOSE` | Enable verbose logging |

## Platform-Specific Configuration

### Linux

Gosync works out of the box on Linux. For optimal performance:

```bash
# Tune network buffers
sudo sysctl -w net.core.rmem_max=16777216
sudo sysctl -w net.core.wmem_max=16777216

# Increase file descriptor limits
ulimit -n 65536
```

### macOS

On macOS, you may need to increase the maximum number of open files:

```bash
ulimit -n 10240
```

### Windows

See [Windows Documentation](README.windows.md) for Windows-specific configuration.

## Example Configurations

### High-Speed Network (40 Gbps+)

```json
{
  "workers": 16,
  "transport": "quic",
  "checksum": true,
  "compression": false,
  "progress": true,
  "verbose": false
}
```

### Conservative Transfer

```json
{
  "workers": 4,
  "transport": "tcp",
  "checksum": true,
  "compression": true,
  "progress": true,
  "verbose": false
}
```

### Development/Testing

```json
{
  "workers": 2,
  "transport": "tcp",
  "checksum": false,
  "compression": false,
  "progress": true,
  "verbose": true,
  "dry_run": true
}
```

## Configuration Validation

Gosync validates configuration on startup. If you provide invalid values, it will:

1. Print an error message
2. Show the problematic option
3. Exit with a non-zero exit code

Example:

```bash
$ gosync --workers -1 source destination
Error: workers must be a positive integer
```

## Debugging Configuration

To see the effective configuration:

```bash
gosync --verbose source destination
```

This will print the resolved configuration values at startup.
