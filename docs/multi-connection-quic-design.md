# Design: multi-socket QUIC fan-out for a single file

Status: **proposed**, not implemented. Chorus task `c29d5af7-459a-4241-9c90-b27f132302b4`.

This specifies the wire protocol, offset handling, completion acknowledgement, integrity
semantics and failure behavior for transferring one file over several QUIC connections.
It deliberately stops short of implementation so the framing can be reviewed first.

## 1. Motivation and evidence

Measured on ai01-1010, 8 GiB per trial, medians of three, all trials byte-exact
(`benchmark-results/quic-matrix/connscale-findings.md`):

| Topology (sockets / connections / streams) | N=1 | N=8 | Scaling |
|---|---:|---:|---:|
| N connections, N sockets | 901.58 | **2179.18** | **2.42×** |
| 1 socket, 1 connection, N streams | 906.69 | 921.25 | 1.02× |
| 1 socket, N connections | 881.51 | 907.19 | 1.03× |
| TLS over TCP, N sockets | 2063.14 | 2913.20 | 1.41× |

A third experiment varied the endpoints independently and settled where the limit lives
(same method, one session, 33/33 trials byte-exact):

| Topology at N=8 | MiB/s | Versus 1/1 |
|---|---:|---:|
| 1 sender socket, 1 receiver socket | 999.52 | 1.00× |
| 1 sender socket, 8 receiver sockets, 8 ports | 887.60 | no gain |
| **8 sender sockets, 1 receiver socket, 1 port** | **1901.15** | **1.90×** |
| 8 sender sockets, 8 receiver sockets, 8 ports | 2106.75 | 2.11× |
| 8 sender sockets, 8 receiver sockets, **1 port, SO_REUSEPORT** | 2158.36 | 2.16× |

**The sender's socket carries the scaling.** Every single-sender-socket cell landed between
887 and 1018 MiB/s regardless of receiver topology. One receiver socket already sustains
90.2% of the symmetric result, and `SO_REUSEPORT` on a single port matches N distinct ports
(102.4% at N=8, within trial spread).

Connection-to-socket distribution under `SO_REUSEPORT` was measured, not assumed, and was
uneven in every trial (for example 8 connections landing as `[1,1,3,1,0,0,0,2]`). It did not
matter here: connections sharing a socket ran at 271.7–274.6 MiB/s against 270.8–277.2
across all sockets, so there was no sharing penalty at these rates. Distribution is
kernel-dependent and could change under a different kernel or an eBPF reuseport program.

A standalone harness with no file protocol, disk I/O or checksum reproduced gosync's
single-connection QUIC rate within 0.09% (841.21 versus 841.93 MiB/s), so the ceiling is in
the per-connection QUIC datapath rather than in gosync's protocol or storage layers. Extra
**streams** and extra **connections over one socket** do not scale; **socket count** does. A
256 MiB connection window changed nothing, so shared-window starvation is not the cause.

Therefore the fan-out worth building is **multiple sender UDP sockets against a single
receiver port** backed by an `SO_REUSEPORT` socket group. `N=4` captured 92% of the `N=8`
gain, so a small fan-out is the target operating point.

Caveats carried forward: the mechanism is inferred from topology without CPU profiling;
reuseport distribution is kernel-dependent and only one kernel was tested; one TLS-over-TCP
socket still beats four QUIC sockets, so `-transport tcp` remains the faster option where
its unencrypted data path is acceptable; and all figures come from a memory-source harness,
so real gosync fan-out adding disk I/O, offset bookkeeping and framing should be expected
to land below every number above.

## 2. Scope

In scope: QUIC sender (`pkg/transport/quic.go`), QUIC receiver (`pkg/server/server.go`),
port allocation in `pkg/deploy/deploy.go`, one new CLI flag.

Out of scope: TCP/server/SSH transports, the multi-file worker pool, compression, resume,
and any change to existing single-connection framing.

Non-goals: no new authentication model, no cross-file striping, no change to the
`-checksum` digest algorithm, no reduction in TLS verification.

## 3. Configuration

New flag, defaulting to current behavior:

```text
-connections N   QUIC data connections per file (1–16, default 1)
```

- `1` reproduces today's wire behavior exactly, including command names.
- Values above 1 are only honored for `-transport quic`; with any other transport the flag
  is rejected rather than silently ignored, matching how `-transport ssh -checksum=true` is
  handled today.
- `-connections` multiplies sockets **per file**, while `-workers` multiplies files in
  flight. Total sockets must be bounded: the implementation caps concurrent data sockets at
  `min(workers × connections, 16)` and reduces per-file fan-out to respect that cap. Small
  files gain nothing from fan-out, so files below a threshold (proposed 64 MiB) use a single
  connection regardless of the flag.

## 4. Wire protocol

### 4.1 Capability discovery

The existing `CAPS` exchange must not change: the client requires the exact response
`OK CAPS NOHASH`, so extending that string would break negotiation against current peers.
Fan-out is discovered with a separate command on the initial control connection:

```text
C: FANOUT
- `OK FANOUT <maxConnections> <port>
S: OK FANOUT <maxConnections> <port>
```

A single UDP port is advertised, served by an `SO_REUSEPORT` socket group presenting the
same certificate as the control connection. A receiver that does not implement fan-out
replies with its existing error for unknown commands; the client then transfers over one
connection without further attempts. There is no partial fallback and no retry loop.

The client uses `min(requested, maxConnections)` sender sockets, and dials only the
advertised port.

### 4.2 Ranged send

Each data connection carries one chunk per stream:

```text
C: SEND-RANGE <token> <offset> <length> <total> <base64path>
   ... exactly <length> payload bytes, then FIN ...
S: OK RANGE <offset> <length>
```

With `-checksum` enabled the client appends its per-range digest and the receiver must echo
the digest it computed over the same bytes:

```text
C: SEND-RANGE-HASH <token> <offset> <length> <total> <base64path>
S: OK RANGE <offset> <length> <hexSHA256>
```

Framing rules:

- `token` is a 128-bit value from `crypto/rand`, hex-encoded, generated per file transfer.
- `offset`, `length`, `total` are decimal, parsed with the existing digit-only `parseSize`.
- The receiver rejects `length == 0`, `offset < 0`, `offset + length > total` and any
  arithmetic overflow, and enforces `total` identical across every range for a token.
- `base64path` reuses today's base64 path encoding and existing path-safety checks; the
  destination path must be identical for every range sharing a token.
- Ranges must be disjoint. A range overlapping an already-written range is refused rather
  than written, so a confused or hostile sender cannot corrupt committed bytes.
- The receiver bounds the number of live tokens and the number of ranges per token, and
  refuses further ranges past those limits.

Chunking is computed by the sender as `ceil(total / connections)` aligned up to the copy
buffer size, giving each connection one contiguous span. Contiguous spans keep receiver
writes sequential per socket, which matters on rotational destinations.

### 4.3 Completion

After every range is acknowledged, the client commits on the **control** connection:

```text
C: COMMIT <token> <total>
S: OK COMMIT <total>
```

The receiver commits only if all of the following hold; otherwise it returns an error and
discards the transfer:

1. The acknowledged ranges exactly cover `[0, total)` with no gap and no overlap.
2. The staged file's size equals `total`.
3. No range for the token is still in flight.

On success the receiver flushes and `fsync`s the staged file, atomically renames it into
place, and drops the token. `COMMIT` is idempotent for an already-committed token so a lost
acknowledgement does not produce a spurious failure.

### 4.4 Abort

```text
C: ABORT <token>
S: OK ABORT
```

`ABORT` removes the staged file and the token. A receiver that never receives `COMMIT` or
`ABORT` expires the token after an idle timeout (proposed: the existing transport timeout,
minimum 60 s) and removes its staged file, so a crashed sender cannot accumulate garbage.

## 5. Receiver implementation

- **Staging.** Ranged transfers write to `<dest>.gosync-<token>.part` in the destination
  directory, never to `<dest>` directly, so an interrupted fan-out never leaves a
  partially written file at the real path. Non-ranged `SEND` keeps its current behavior.
- **Offset writes.** The staged file is opened once per token and shared by the connections.
  Each stream writes through an `io.Writer` that wraps `(*os.File).WriteAt` at a fixed
  offset, advancing its own cursor only. No shared seek position is used, so concurrent
  writes need no write lock; only the token registry is mutex-protected.
- **Preallocation.** The receiver truncates the staged file to `total` on first range.
  `fallocate` is preferable where available but must not be required, since tmpfs and some
  filesystems reject it; failure to preallocate is not fatal.
- **Listening sockets.** The QUIC receiver currently creates one `quic.Listener`. Because
  receiver sockets contribute only ~9–11%, the receiver keeps **one port**. It binds a
  group of `N` sockets to that single port with `SO_REUSEPORT` (via `net.ListenConfig`'s
  `Control` hook), each with its own `quic.Listener`, sharing one `tls.Config` and one
  certificate. Accepted connections are served identically regardless of which socket
  receives them, so uneven kernel distribution is not a correctness concern.
  A receiver that cannot set `SO_REUSEPORT` falls back to a single socket on that port,
  which measurement shows still delivers 90.2% of the symmetric gain. Distinct ports are
  therefore not needed and are not implemented.
- **Deployment.** `pkg/deploy/deploy.go` keeps its existing single-port probe in
  49152–65535 unchanged. This is a direct consequence of the asymmetric measurement: no
  port-set allocation, no extra firewall surface, and no change to the deployment contract.

Sender side: `N` `quic.Transport` instances, each on its own `net.UDPConn`, each dialing the
receiver's single advertised port. This is the dimension that actually scales.

## 6. Integrity semantics

With `-checksum` **disabled** (the default), fan-out gives the same guarantee as today's
checksum-off path plus exact coverage: QUIC's AEAD protects each connection's bytes,
per-range acknowledgements confirm the received length, and `COMMIT` proves the ranges
tile `[0, total)` exactly once and the staged size matches.

With `-checksum` **enabled**, each range is verified end to end by digest. One limitation
must be stated rather than papered over: **SHA-256 of the whole file cannot be composed
from per-range digests**, so per-range verification alone does not establish whole-file
digest equality.

Therefore `-checksum` with `-connections > 1` performs a **whole-file read at commit**:

```text
C: COMMIT-HASH <token> <total> <hexSHA256>
S: OK COMMIT <total> <hexSHA256>
```

After coverage and size checks pass, and before the rename, the receiver reads the staged
file back from start to finish, computes SHA-256 over it, and compares against the digest
the sender computed over the source. A mismatch aborts the commit and removes the staged
file, so a file that fails verification never appears at the destination path. This gives
ranged transfers the same end-to-end whole-file guarantee as single-connection
`-checksum`, with one addition worth noting: it verifies what was actually persisted,
including correct offset placement, not merely what crossed the wire.

Cost and behavior to document in `docs/configuration.md`:

- The receiver reads back the entire file, so commit time grows with file size and is
  bounded by destination read throughput. On a RAM disk this is cheap; on slow storage it
  can be a significant share of total transfer time.
- Hashing returns to the receiver's CPU at commit, which matters on hosts without SHA-NI
  (agrippa-1010), where hashing was historically the bottleneck.
- Per-range digests are still exchanged, so a corrupt range is detected during transfer
  rather than only at commit.
- Checksum-off transfers never read the file back and pay none of this cost.

## 7. Security

- Every data connection performs the same TLS verification and certificate pinning as the
  control connection; pinning is not relaxed for additional sockets.
- The token is not an authentication mechanism. It scopes ranges to one transfer and makes
  accidental cross-transfer collisions negligible. A receiver reachable by an untrusted
  party is as exposed as it is today; fan-out neither improves nor worsens that, and the
  documentation must not imply otherwise.
- Additional listening ports widen the receiver's exposed surface. The deployed receiver
  should bind the same interface as today and advertise only ports it actually opened.

## 8. Failure semantics

- A range failing mid-transfer is retried on another connection from its original offset,
  up to the existing retry budget. Retry is safe because ranges are idempotent writes at
  fixed offsets, and an overlapping *acknowledged* range is refused.
- If any range cannot complete, the client sends `ABORT` and reports failure. No partial
  file appears at the destination path.
- If a data connection dies, its outstanding range is reassigned rather than failing the
  transfer, provided the control connection is alive.
- If the control connection dies, the transfer fails and the receiver's token timeout
  performs cleanup.
- Progress accounting sums bytes across connections through the existing progress callback,
  which must become safe for concurrent calls.

## 9. Testing plan

Unit: range coverage validation (exact tile, gap, overlap, duplicate, zero length, negative,
overflow, mismatched `total`, mismatched path), token limits, offset-writer behavior,
`FANOUT` parsing including malformed responses, commit idempotence, abort cleanup, flag
validation including rejection with non-QUIC transports, and `COMMIT-HASH` verification
covering both a matching digest and a deliberately corrupted staged file that must abort
without leaving a destination file.

Integration: loopback transfers for `connections` 1–8 against a real receiver, verifying
byte-exact output and staged-file cleanup; interrupted-transfer test asserting no file at
the destination path; old-receiver compatibility test asserting single-connection fallback;
`go test -race ./...`.

Live: repeat the 6,140,975,104-byte ISO on ai01-1010 and agrippa-1010 with RAM-disk
destinations, `connections` in {1, 2, 4, 8}, three serial trials each, independent
size/SHA-256 verification outside timing, compared against the recorded single-connection
and TCP medians.

## 10. Expected outcome and open questions

The harness suggests roughly 842 → ~2,000 MiB/s on ai01 at four to eight sockets. The real
implementation adds disk writes, offset bookkeeping and protocol overhead, so it should be
expected to land **below** the harness numbers. No performance claim should be made for
gosync until the live matrix above is run.

Resolved by the asymmetric experiment:

- The **sender's** socket count carries the scaling; receiver sockets are worth ~9–11%.
- `SO_REUSEPORT` on **one** receiver port matches N distinct ports, so the receiver keeps a
  single port and deployment is unchanged.

Open questions remaining:

1. Default value for `-connections` once measured against real gosync; it stays `1` until
   then. Automatic selection is tracked separately as "Auto-tune QUIC connection fan-out
   count" (`5dc8e7fd-afeb-483a-b97c-235f5f9ae837`) and is deliberately out of scope here.
2. Whether the 64 MiB fan-out threshold is the right cutover point.
3. Interaction with `-resume`, which currently reasons about whole files.
4. Whether commit-time whole-file verification should also be offered for
   single-connection transfers, where it would likewise catch persistence errors.
5. Behavior on kernels or platforms where `SO_REUSEPORT` is unavailable; the single-socket
   fallback is specified but unmeasured outside Linux.
