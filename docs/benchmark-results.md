# Agrippa RAM-disk comparison and profiles

## Checksum semantics and historical evidence

All gosync production network measurements recorded here predate the effective
checksum-off change and **hashed at both endpoints**, even where the flag was
false or omitted. Preserve those results as historical hashing measurements;
they are not evidence for the new no-hash mode. Synthetic copy-only figures are
also not end-to-end no-hash protocol measurements. No new performance claim or
no-hash speedup is established by this documentation update.

Today `-checksum=false` (the default) disables application SHA-256 at both
QUIC/TCP/server endpoints, including with `-deploy`. During `Connect`, a separate
`CAPS` exchange must return exactly `OK CAPS NOHASH` before using `SEND-NOHASH` /
`RECEIVE-NOHASH`. Size framing and acknowledgements remain, but no digest is
computed or verified. Unsupported receivers fail before file data: upgrade the
receiver, or use `-checksum` only with immediate pre-CAPS size-first/base64 SHA-256
peers. There is **no fallback**. True retains `SEND <size> <base64path>` / `RECEIVE`
with strict SHA-256 digest and size/framing/ACK/trailer checking. Compatibility
does not extend to arbitrary older versions or the historical HEAD receiver's
path-first `SEND <literal-path> <size>` protocol. Those peers must **upgrade both
ends**; no protocol auto-detection or compatibility is promised. Size-first/base64
framing was already present before the checksum-mode change, not introduced by it. The receiver selects
mode per request; no global `serve` checksum flag is needed, and the deploy client
dictates it.

QUIC TLS encryption, certificate verification, and leaf pinning are unchanged in
either mode. TCP remains unencrypted and has no application digest with false.
SSH transport integrity is separate; standalone SSH now rejects `-checksum=true`
rather than silently ignoring it. See [explicit on/off commands and migration](configuration.md#application-checksum-mode-and-compatibility).
The network runner below explicitly uses `-checksum`, so reruns remain hash-enabled;
the [local benchmark's zero-value configuration](benchmark-internals.md#scope-and-interpretation)
requires separate care when comparing new runs with old data.

## Setup

Recorded 2026-09-09 against the working tree at each measurement, including
uncommitted fixes—not necessarily today's implementation. **All earlier gosync
network measurements in this report used TCP**, including asynchronous hashing,
buffer-size trials, and flag validation. Synthetic hashing/copy measurements use
no transport. None of these figures are QUIC performance results.
The runner used the deployed **ServerTransport TCP path**, not QUIC. SSH stages
an isolated receiver once; this deliberately avoids `-deploy`'s global process
cleanup. The initial baseline did not change production buffer sizes, hashing, protocol,
or defaults. The asynchronous-hashing follow-up is documented separately below.

Remote host: `agrippa-1010` (hostname `agrippa`), Intel Xeon E5-2697A v4 @ 2.60 GHz.
Local profiling host: AMD Ryzen 9 7950X, Go 1.26.0 linux/amd64.
Source: `/home/gbjohnso/CD_Images/Win10_22H2_English_x64v1.iso`, **6,140,975,104 bytes**.

A dedicated tmpfs was mounted at **`agrippa-1010:/mnt/gosync-bench`**:

- Capacity: **10,737,418,240 bytes (10 GiB)**, allocated on demand, not reserved.
- Options: `noswap,nodev,nosuid,noexec,mode=0700,uid=1000,gid=1000`.
- The host had approximately 17–18 GB available memory during setup/final checks.
- No `/etc/fstab`, global swap, cache-dropping, or network settings changed.
- Temporary benchmark files/processes were cleaned up; the mount remains empty
  and available for subsequent transfers. It is volatile and not reboot-persistent.
- `/tmp` on this host was already tmpfs; the original user's `/tmp` test was
  already RAM-backed, though that mount permits swapping.

## Reproduce the network comparison

From the repository root:

```sh
go build -o gosync ./cmd/gosync
python3 scripts/benchmark-transfer.py \
  --host agrippa-1010 \
  --source /home/gbjohnso/CD_Images/Win10_22H2_English_x64v1.iso \
  --destination /mnt/gosync-bench \
  --gosync ./gosync \
  --output ./benchmark-results/transfer \
  --runs 3 --warmups 1 --timeout 120
```

See [runner details](benchmark-transfer.md). Both tools write the same isolated
`payload` path on tmpfs, removing the previous trial's output first. The source
is warmed by SHA-256 hashing; each tool has one warm-up transfer. Measured order
is gosync/rsync, rsync/gosync, gosync/rsync. Only one ISO output exists at a time.
Every trial is verified against the source SHA-256 outside transfer timing.

Rsync uses SSH with compression disabled, `--whole-file`, `--ignore-times`,
`--no-compress`, and no progress UI. Gosync uses `-transport server -workers 1
-checksum -progress=false`. Both commands' wall times include their connection
and process startup, but exclude receiver staging, verification, and cleanup.
These are **full-transfer**, warm-source measurements, not rsync delta/skip tests,
QUIC tests, or durable disk/fsync tests. Rsync still encrypts; gosync TCP does not.

## Measured results

Decimal MB/s = bytes / seconds / 1,000,000. MiB/s = bytes / seconds / 1,048,576.

| Tool | Round | Wall seconds | MB/s | MiB/s |
| --- | ---: | ---: | ---: | ---: |
| gosync | 1 | 24.709 | 248.53 | 237.02 |
| rsync | 1 | 17.667 | 347.60 | 331.50 |
| rsync | 2 | 16.413 | 374.15 | 356.82 |
| gosync | 2 | 24.869 | 246.93 | 235.49 |
| gosync | 3 | 23.136 | 265.43 | 253.13 |
| rsync | 3 | 17.048 | 360.22 | 343.53 |
| **gosync median** | | **24.709** | **248.53** | **237.02** |
| **rsync median** | | **17.048** | **360.22** | **343.53** |

Gosync delivered approximately **31% lower median throughput**; equivalently,
rsync delivered **45% greater throughput**. All eight transfers, including
warm-ups, passed SHA-256 and length verification.

Raw commands, outputs, versions, environment, timings, and cleanup logs:
`benchmark-results/transfer/benchmark-transfer-9acc9163e0514fcda5193aff56c5abbe.json`.
This initial report predates subsequent cleanup-hardening of the Python runner;
transfer commands and timing methodology remain unchanged.

### CPU during the actual network transfer

Seconds of CPU, not percentages of wall time; totals can exceed elapsed time
because a process has multiple threads. Remote CPU comes from receiver
`/proc/PID/stat` snapshots outside the timed transfer.

| Gosync round | Sender user | Sender system | Receiver user | Receiver system |
| --- | ---: | ---: | ---: | ---: |
| 1 | 2.819 | 3.118 | 21.11 | 6.12 |
| 2 | 2.907 | 2.229 | 20.87 | 6.59 |
| 3 | 2.534 | 1.969 | 20.96 | 4.98 |

The receiver expends much more CPU than the sender. This points toward receiver
CPU costs, but does not by itself assign those costs to hashing or system calls.

## Isolated hashing/buffer measurements on Agrippa

The exact local `benchmark-results/transport.test` binary was copied to a unique
remote staging directory and executed on Agrippa. Synthetic 8 MiB transfer loops
were measured with `-test.cpu=1`, `-test.benchtime=500ms`, and `-test.count=3`.
The destination is a preallocated in-memory buffer, not a socket or file.

| Buffer | Copy-only median MB/s | SHA-256 median MB/s |
| --- | ---: | ---: |
| 32 KiB | 9,133.76 | 339.83 |
| 256 KiB | 6,262.06 | 331.83 |
| 1 MiB | 3,966.16 | 329.47 |

A separate three-second profile of the 32 KiB hashing case measured 345.25 MB/s.
Its CPU samples were **96.38% `sha256.blockAVX2`** and **2.96% `runtime.memmove`**.
The Ryzen's corresponding local profile instead used `sha256.blockSHANI`;
local synthetic hashing throughput was about 1,863 MB/s at 32 KiB.

The remote results and CPU profile are retained in:

- `benchmark-results/agrippa/matrix.txt`
- `benchmark-results/agrippa/sha256.txt`
- `benchmark-results/agrippa/sha256.cpu`
- `benchmark-results/agrippa/sha256-top.txt`

Inspect using the matching retained binary:

```sh
go tool pprof -top benchmark-results/transport.test \
  benchmark-results/agrippa/sha256.cpu
```

Remote benchmark selectors (run a compiled `transport.test` on the remote host):

```sh
./transport.test -test.run='^NONE' \
  -test.bench='^BenchmarkTransferLoopSynthetic$/buffer=/SHA256=/^ProgressAtomic=false$' \
  -test.benchtime=500ms -test.count=3 -test.cpu=1 -test.benchmem -test.timeout=90s
./transport.test -test.run='^NONE' \
  -test.bench='^BenchmarkTransferLoopSynthetic$/^buffer=32KiB$/^SHA256=true$/^ProgressAtomic=false$' \
  -test.benchtime=3s -test.cpu=1 -test.timeout=30s -test.cpuprofile=sha256.cpu
```

See [internal benchmarks](benchmark-internals.md) for full local loopback and
synthetic methodology, allocations, and reproducible profile commands. Those
local loopback profiles include both endpoints, not just the remote receiver.

## Follow-up: asynchronous SHA-256 (2026-09-09)

Implemented `checksum.CopyNWithSHA256` and wired it into TCP/ServerTransport
send/receive methods and the server's SEND/RECEIVE handlers. Four owned 32 KiB
buffers circulate between synchronous I/O and one ordered hashing goroutine.
There is no extra payload copy, memory is bounded at 128 KiB per active copy,
and all returns drain/join the worker. Transfers still use the same SHA-256 and
protocol, and success waits for final checksum completion/verification.
SSH/QUIC implementations were outside that historical change. QUIC now uses
the shared asynchronous pipeline too when checksums are enabled; this follow-up
measured TCP only, with hashing active.

Using the same ISO, RAM disk, runner, and warm-up/alternating three-round method:

| Implementation | Median seconds | Median MB/s | Median MiB/s |
| --- | ---: | ---: | ---: |
| Earlier synchronous gosync | 24.709 | 248.53 | 237.02 |
| Asynchronous gosync | 22.739 | 270.06 | 257.55 |
| Rsync in asynchronous comparison | 17.106 | 359.00 | 342.37 |

Async gosync trials: 22.739 / 22.473 / 23.296 seconds, or
270.06 / 273.26 / 263.61 MB/s. All eight transfers, including warm-ups and
rsync controls, passed SHA-256 verification. Temporary receiver/data cleanup
completed; the RAM disk remains mounted.

This is **8.7% higher median throughput** than the earlier synchronous run,
not a simultaneous randomized A/B test of the two gosync binaries. Rsync's
control median remained close (360.22 to 359.00 MB/s), but machine load and
sampling variation are still possible. The serial hash limit remains.

Raw follow-up report:
`benchmark-results/async-transfer/benchmark-transfer-e66a8c51096244c28e926addc758b520.json`.
The pre-change executable is retained as `benchmark-results/gosync-sync-hash`.

Helper-only memory-copy benchmarks are available separately:

```sh
go test ./pkg/checksum -run '^$' -bench CopyN -benchmem -count=3 -timeout=60s
```

These do not model socket/disk overlap; asynchronous execution is not guaranteed
to improve pure memory-copy workloads. Correctness tests gate hashing to verify
overlap, bounded backpressure, worker draining, buffer ownership, and precise
protocol framing. Truncated transfers return errors instead of success.

## Follow-up: end-to-end asynchronous buffer sizes

Tested the full 6,140,975,104-byte ISO on the same Agrippa tmpfs. The user explicitly
authorized clearing its test contents before this run. Both endpoints ran the
same experimental binary. Go build overlays changed only the original async
helper's buffer-size constant, with four buffers per active copy throughout.
These were TCP/server transfers, **not QUIC**.

Three rounds balanced each size's position:

1. 32 KiB, 256 KiB, 1 MiB
2. 256 KiB, 1 MiB, 32 KiB
3. 1 MiB, 32 KiB, 256 KiB

Each matrix cell used the runner with `--runs 1 --warmups 0`; it warmed the source
by hashing, staged one isolated receiver, transferred with gosync, then ran a
full rsync control. Setup, verification, and cleanup were excluded from the
command timer. This differs from the earlier multi-round warm-up methodology;
compare buffer variants within this matrix, not across all historical runs.

| Buffer per slot | Round 1 MB/s | Round 2 MB/s | Round 3 MB/s | Median MB/s | Median seconds | Change from 32 KiB | Payload buffers per endpoint |
| --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| 32 KiB | 273.87 | 265.78 | 268.78 | **268.78** | 22.847 | baseline | 128 KiB |
| 256 KiB | 284.36 | 287.42 | 292.69 | **287.42** | 21.366 | **+6.9%** | 1 MiB |
| 1 MiB | 288.03 | 305.26 | 289.87 | **289.87** | 21.185 | **+7.8%** | 4 MiB |

All nine gosync and nine rsync transfers passed SHA-256/length verification.
The rsync controls ranged from 354.79 to 396.34 MB/s, so host variability remains
visible. All larger-buffer gosync trials exceeded all 32 KiB trials. This is a
repeatable practical gain in this small sample, not a formal significance test
or guarantee across workloads. 1 MiB was only ~0.9% faster than 256 KiB by median,
with four times the buffer storage.

Raw reports and exact historical binaries/overlays are retained under
`benchmark-results/buffer-sizes/round{1,2,3}-{32,256,1024}/` and its parent.
Overlay snapshots are retained as `.go.txt` to avoid accidental compilation by
`go test ./...`. They describe the pre-flag source and are historical artifacts,
not overlays to apply to the new configurable API.

The gain justified adding **`--buffer-size BYTES`** while preserving the 32 KiB
default. The range is 4 KiB–4 MiB. Deployment sets both endpoints; manual servers
must be configured independently. The hash-enabled queue remains four buffers deep;
today's no-hash mode uses one buffer per active copy (32 KiB by default, up to
4 MiB), rather than four (128 KiB by default, up to 16 MiB per endpoint). No socket
SO_SNDBUF/SO_RCVBUF setting or checksum algorithm changed. The size is the maximum
payload chunk; socket reads may return less.

Reproduce one configurable-size run with the final binary:

```sh
python3 scripts/benchmark-transfer.py \
  --host agrippa-1010 \
  --source /home/gbjohnso/CD_Images/Win10_22H2_English_x64v1.iso \
  --destination /mnt/gosync-bench --gosync ./gosync \
  --output ./benchmark-results/buffer-validation \
  --buffer-size 262144 --runs 3 --warmups 1 --timeout 120
```

A one-round full-ISO validation of the final flag-enabled sender and receiver
measured **282.91 MB/s** (21.706 s); rsync measured 357.03 MB/s. Both verified.
Its report is in `benchmark-results/buffer-sizes/flag-validation/`. This extra
run validates flag wiring and is not included in the comparison medians.

### Historical QUIC/UDP verification and current status

The earlier `TestQUICTransportSendStreamOverUDP` used a compatible quic-go test
peer and transferred 256 KiB + 17 bytes, checking the prototype SEND header,
payload, client SHA-256 trailer, and stream EOF. It passed repeated race tests
at that stage. That was a standalone-client correctness check, not a performance
measurement; the production receiver was TCP-only **at that time**.

The current production receiver supports TCP or QUIC via `server.SetTransport`
and `gosync serve --transport tcp|quic` (`serve` still defaults to TCP). Transfer
commands default to QUIC, including `-deploy`, which now honors `quic`, `tcp`, or
`server` while using SSH only for bootstrap/cleanup; deploy with `ssh` or an
unknown transport is rejected. Runtime output no longer reports a TCP override;
server startup logs identify the actual bound address/network, including UDP.

QUIC uses shared size-first/base64-path `SEND`/`RECEIVE` framing and SHA-256
verification when checksums are enabled, or capability-gated no-hash commands
when disabled. Hash-enabled copies use four buffers, 32 KiB each by default;
no-hash copies use one. The configurable range is 4 KiB–4 MiB per buffer. This
replaces the old experimental QUIC wire format (including the old client checksum
trailer), so both binaries must be updated. Compatibility with pre-CAPS
size-first/base64 TCP SHA-256 peers is retained with `-checksum=true`, not with
the new default no-hash mode. Older path-first peers require both ends to be
upgraded.

At the time of the historical QUIC validation below, the client skipped
certificate verification. That run does not validate today's TLS trust behavior.
Current QUIC uses ALPN `gosync`, encrypts traffic, and authenticates the receiver
via system PKI or an exact leaf pin; deployment retrieves and pins the public leaf
over host-key-verified SSH. These protections are unchanged by checksum mode.
There is still no client authentication: restrict receiver network access. See
[QUIC security configuration](configuration.md#quic-security-and-compatibility).

The benchmark runner still uses `-transport server` and TCP readiness checks;
see [runner scope](benchmark-transfer.md) before attempting a new transport
comparison. The production QUIC deployment validation below is separate.

### Production QUIC deployment validation

Historically ran the actual CLI against Agrippa after rebuilding both endpoints,
with application hashing active (before the effective checksum-off change):

```sh
./gosync -deploy -transport quic -deploy-listen 0.0.0.0:60321 \
  -buffer-size 262144 -checksum -progress=false \
  /home/gbjohnso/CD_Images/Win10_22H2_English_x64v1.iso \
  agrippa-1010:/mnt/gosync-bench/
```

- CLI selected `quic (UDP); deployment: SSH` with no override.
- The deployed server logged `[::]:60321 (QUIC/UDP)`.
- Transferred **6,140,975,104 bytes**, one completed file, zero failed files.
- The protocol's checksum acknowledgement succeeded. An independent local and
  remote `sha256sum` then matched:
  `a6f470ca6d331eb353b815c043e327a347f594f37ff525f17764738fe812852e`.
- A bounded remote tcpdump on `udp port 60321 or tcp port 60321` captured **200 UDP
  observations and no TCP packets**. Capture used `-i any`, so bridge/interface
  observations can duplicate packets. This sampled the start of the transfer,
  not the entire file; it is not a wire-byte or throughput measurement.
- Transfer summary: **25.662 seconds**, **228.22 MiB/s** (gosync still labels this
  MB/s). This single validation run is not a controlled QUIC-vs-TCP benchmark or
  evidence that QUIC is faster. SSH deployment/cleanup are outside that summary.
- Deployment cleanup removed the remote receiver and staged executable. The test
  ISO was removed after independent verification; the RAM disk remains mounted.

Artifacts retained under `benchmark-results/`:
`quic-deploy-transfer.txt`, `quic-deploy-packets.txt`,
`quic-deploy-source.sha256`, and `quic-deploy-receiver.txt`.
A subsequent 8 MiB smoke transfer without either `-transport` or `-deploy-listen`
also succeeded: default QUIC selected an available UDP port (58280) and deployed
the QUIC receiver. Its log is `quic-deploy-default.txt`.

QUIC/TCP lifecycle, concurrent streams, error acknowledgements, truncated data,
checksums, paths with spaces, and configured buffers are covered by tests.
At that validation stage, `go test -race ./...` passed without skipping the former
listener-startup race test; listener lifecycle synchronization was updated to support both
protocols. Failed file transfers now propagate a nonzero CLI exit rather than
being masked by a final summary.

## Interpretation and next steps

The historical hash-enabled evidence strongly supports **serial receiver SHA-256
as a major bottleneck** on Agrippa; it does not characterize today's no-hash mode. Its isolated hash loop reaches roughly 340 MB/s before socket/file
handling; the actual receiver spends ~21 seconds in user CPU for a 23–25 second
transfer. This explains why a faster source machine or more file workers doesn't
solve a single-ISO transfer. It is not a direct CPU profile of the cross-host
receiver process, so the exact share of its wall time is still unmeasured.

Larger buffers did not improve the synthetic hash loop, but the later end-to-end
pipeline experiment above found a modest ~7–8% benefit. That distinction supports
measuring the real transfer path instead of inferring socket performance from
memory-only benchmarks.

Possible future work, **not implemented here**:

1. Profile the actual network receiver after the bounded-pipeline change to
   quantify remaining CPU costs and time waiting on the hash worker.
2. Consider a negotiated alternative checksum algorithm if preserving SHA-256
   compatibility is not required; do not silently weaken verification.
3. Expand the buffer-size sample across workloads and host loads before changing
   the conservative default.

To remove the RAM disk later, first ensure it contains no wanted data and no
transfers are using it, then run:

```sh
ssh agrippa-1010 'sudo umount /mnt/gosync-bench && sudo rmdir /mnt/gosync-bench'
```

Unmounting destroys any data still on that tmpfs. It has deliberately been left
mounted for the user.
