# Internal transfer benchmarks and CPU profiles

## Scope and interpretation

The recorded figures below are the original **synchronous-hashing baseline**.
Historical production network transfers always hashed at both endpoints, even
when the checksum flag/configuration was false. These figures must not be relabeled
as no-hash measurements. The currently inspected `BenchmarkServerTransportTCP`
constructs `transport.Config{Timeout: 5}` without `Checksum`, whose zero value is
now false: rerunning it exercises the new no-hash path, not this historical
hashing baseline. It has no checksum-mode benchmark selector. An explicit
`Checksum: true` in the benchmark setup would be required to measure today's
asynchronous hash-enabled path; this documentation update does not change code.
Record the actual configuration and binary with any new results. See
[the asynchronous follow-up](benchmark-results.md#follow-up-asynchronous-sha-256-2026-09-09)
for remote results and `pkg/checksum/copy_sha256_benchmark_test.go` for an explicit
sync/async helper comparison. Synthetic loop variants below remain synchronous.

The original benchmark addition changed benchmark code only, not production
protocol, checksums, buffer sizes, or defaults. That is historical scope, not a
claim that today's production implementation is unchanged. These local benchmarks
involve no remote commands, rsync runs, or remote RAM-disk setup.

All recorded network measurements here used **TCP**, not QUIC; synthetic loops
and helper benchmarks use no network transport. Production now also supports
`server.SetTransport("quic")` / `gosync serve --transport quic` over UDP and the
same configurable copy helpers (4 KiB–4 MiB per buffer): checksum-on uses four
32 KiB buffers by default and an ordered SHA-256 worker; checksum-off uses one
32 KiB buffer by default and no hashing at either endpoint. Only pre-CAPS
size-first/base64 TCP SHA-256 peers remain compatible with checksum-on; older
path-first peers require both ends to be upgraded. No-hash commands require
receiver capability support. The old experimental QUIC client wire format changed, requiring both
binaries to be updated. These TCP and
synthetic figures do not establish QUIC performance. A separate full-ISO deployment
validated production QUIC/UDP; see [results](benchmark-results.md#production-quic-deployment-validation).
See [configuration](configuration.md#quic-security-and-compatibility)
for current QUIC TLS authentication, encryption, and pinning, which are unchanged
by checksum mode. TCP is unencrypted and has no application digest in off mode.
SSH transport integrity is separate; standalone SSH now rejects `-checksum=true`.

For QUIC/TCP/server, including deployment, CLI `-checksum=false` and library
`transport.Config.Checksum == false` select no-hash. `sync.Config.Checksum` is
propagated before `Connect`. A separate `CAPS` exchange must return exactly
`OK CAPS NOHASH` before `SEND-NOHASH` / `RECEIVE-NOHASH` can transfer file data;
size framing and acknowledgements remain. Unsupported peers fail before data:
upgrade the receiver, or use `-checksum` only for immediate pre-CAPS
`SEND <size> <base64path>` / `RECEIVE` SHA-256 peers, with strict digest and
size/framing/ACK/trailer checks. This does not cover arbitrary older versions or
the historical HEAD receiver's path-first `SEND <literal-path> <size>` protocol.
Those peers must **upgrade both ends**; no protocol auto-detection or compatibility
is promised. Size-first/base64 framing was already present before the checksum-mode
change, not introduced by it. There is no fallback. Receivers
select the mode per request; no server-global checksum flag is needed. See
[on/off examples and migration](configuration.md#application-checksum-mode-and-compatibility).
No performance result for the new production no-hash mode is claimed here.

- `pkg/server/server_benchmark_test.go`: `BenchmarkServerTransportTCP` runs the
  actual deployed `transport.ServerTransport` client against `Server.handleClient`
  over a real loopback TCP connection. `SendFile` reaches `handleSend`;
  `ReceiveFile` reaches `handleReceive`. In the recorded baseline, both endpoints
  hashed with the same SHA-256 algorithm and exchanged real commands/checksums;
  today's zero-value setup instead selects no-hash, as noted above.
  `SendFileProgressAtomic` adds a synchronous atomic byte-counter callback.
- `pkg/transport/transfer_loop_benchmark_test.go`:
  `BenchmarkTransferLoopSynthetic` is a **synthetic sender-loop experiment**, not
  a transport or protocol implementation. It uses a `bytes.Reader`, an explicit
  read/write loop, the production `progressWriter`, and a preallocated
  `bytes.Buffer` destination. It compares 32 KiB, 256 KiB, and 1 MiB buffers,
  SHA-256 versus no hashing, and nil versus atomic-counter progress callbacks.
  There are two real memory copies (source to transfer buffer, buffer to sink),
  not an `io.Discard` shortcut. No `io.Copy` fast path or socket scheduling can
  bypass or dominate this experiment.

Each operation transfers a deterministic 8 MiB payload. All cases call
`b.SetBytes(8 << 20)` and `b.ReportAllocs()`. MB/s is Go's decimal payload MB/s,
not aggregate memory traffic or wire bytes. Fixtures, connection establishment,
and final byte-for-byte checks are outside benchmark timing. Synthetic digest
and callback-byte totals are checked after timing; the real send callback total
is also checked. Historical real transfers retained production checksum verification;
current real transfers retain size checks and the final byte-for-byte fixture check,
but application digest verification requires explicitly enabling `Checksum`.

### What is timed

**Historical TCP baseline:** one persistent connection per benchmark invocation,
file open/stat or create/truncate, protocol commands, payload, hashing at both
endpoints, checksum response processing, and file close. A current default-mode
rerun omits hashing/digest verification and processes size-only responses instead;
connection capability negotiation occurs before timing. Source and destination paths are reused.
Allocation metrics include both endpoints because they run in one process.
There is no fsync, network encryption, deployment, reconnect per file, directory
traversal, compression, or sync orchestration in this measurement.

The benchmark owns a listener and calls the production handler for its accepted
connection. It deliberately does not call `Server.Start` or poll `Server.Addr`:
this avoided the listener-startup race and inability to exit cleanly after
listener close in the historical implementation. Cleanup closes the connection/listener
and waits for the handler. A one-minute server connection deadline bounds a stuck
transfer; keep each individual benchmark invocation comfortably below that
limit. The outer Go test timeout provides another bound.

**Synthetic:** a fresh transfer buffer is allocated per operation, matching the
historical synchronous production allocation lifecycle, not today's hash-enabled
four-buffer pipeline. Reader and destination capacity are reused;
sink growth is excluded. The hash-enabled variant creates a hasher and
`io.MultiWriter` per operation and finalizes the digest; the no-hash variant
creates neither. Hex encoding and protocol framing are excluded. Consequently,
this comparison includes hash-wrapper allocation/dispatch costs, not just the
SHA compression function. The memory sink is not a model of kernel TCP writes.

The atomic callback is a small, concurrency-safe progress reporter, not the
application's complete progress/UI pipeline. No rendering, locking, throttling,
contention, or console output is simulated. Callback calls per synthetic
transfer are 256 / 32 / 8 for the three buffer sizes.

## Reproduce locally

Run from the repository root. Use an otherwise idle machine; do not run separate
benchmark processes concurrently. Commands below use the local toolchain and
cached dependencies only (`GOPROXY=off`, `GOTOOLCHAIN=local`); missing dependencies
fail rather than being downloaded. Every benchmark command has a Go timeout;
the recorded runs also had terminal wall-clock bounds of 30–110 seconds.

```sh
mkdir -p benchmark-results
ls -la benchmark-results
export GOCACHE=/tmp/gosync-go-cache
export GOPROXY=off
export GOTOOLCHAIN=local

# Quick correctness/compilation check; skips all ordinary tests.
go test ./pkg/server ./pkg/transport -run '^$' \
  -bench 'Benchmark(ServerTransportTCP|TransferLoopSynthetic)' \
  -benchtime=1x -benchmem -timeout=60s > benchmark-results/smoke.txt 2>&1

# Repeated, unprofiled measurements: 3 samples for all 15 cases.
go test ./pkg/server ./pkg/transport -run '^$' \
  -bench 'Benchmark(ServerTransportTCP|TransferLoopSynthetic)' \
  -benchtime=500ms -count=3 -benchmem -cpu=1 -timeout=90s \
  > benchmark-results/matrix.txt 2>&1

# SHA and copy profiles are separate so each has an interpretable denominator.
go test ./pkg/transport -run '^$' \
  -bench '^BenchmarkTransferLoopSynthetic$/^buffer=32KiB$/^SHA256=true$/^ProgressAtomic=false$' \
  -benchtime=3s -benchmem -cpu=1 -timeout=30s \
  -cpuprofile=benchmark-results/synthetic-sha256.cpu \
  -o benchmark-results/transport.test > benchmark-results/synthetic-sha256.txt 2>&1

go test ./pkg/transport -run '^$' \
  -bench '^BenchmarkTransferLoopSynthetic$/^buffer=32KiB$/^SHA256=false$/^ProgressAtomic=false$' \
  -benchtime=3s -benchmem -cpu=1 -timeout=30s \
  -cpuprofile=benchmark-results/synthetic-copy.cpu \
  -o benchmark-results/transport.test > benchmark-results/synthetic-copy.txt 2>&1

# Combined real sender + receiver profile, without the callback variant.
go test ./pkg/server -run '^$' \
  -bench '^BenchmarkServerTransportTCP$/(SendFile|ReceiveFile)$' \
  -benchtime=3s -benchmem -cpu=1 -timeout=45s \
  -cpuprofile=benchmark-results/server-tcp.cpu \
  -o benchmark-results/server.test > benchmark-results/server-tcp.txt 2>&1

go tool pprof -top -nodecount=15 benchmark-results/transport.test \
  benchmark-results/synthetic-sha256.cpu > benchmark-results/synthetic-sha256-top.txt
go tool pprof -top -nodecount=15 benchmark-results/transport.test \
  benchmark-results/synthetic-copy.cpu > benchmark-results/synthetic-copy-top.txt
go tool pprof -top -nodecount=15 benchmark-results/server.test \
  benchmark-results/server-tcp.cpu > benchmark-results/server-tcp-top.txt

# Optional correctness smoke under race instrumentation, NOT a timing result.
# Benchmark-only: excludes ordinary tests (known-racy in the historical baseline).
go test ./pkg/server ./pkg/transport -race -run '^$' \
  -bench 'Benchmark(ServerTransportTCP|TransferLoopSynthetic)' \
  -benchtime=1x -timeout=60s > benchmark-results/benchmark-race-smoke.txt 2>&1

# Record the environment and dirty-tree context alongside the data.
go version > benchmark-results/environment.txt
uname -a >> benchmark-results/environment.txt
lscpu >> benchmark-results/environment.txt
df -T . /tmp >> benchmark-results/environment.txt
git --no-pager rev-parse HEAD >> benchmark-results/environment.txt
git --no-pager --no-optional-locks status --short >> benchmark-results/environment.txt
ls -lh benchmark-results
```

Retain the matching `.test` binaries with the profiles. The two synthetic runs
use the same unchanged source/build; do not overwrite the binary after changing
code and then use it to symbolize an older profile. CPU profiles include process
setup, calibration iterations, cleanup, and all goroutines, even when the
benchmark timer is stopped. Their sampling percentages are not wall-time shares.
The real server prints connection messages, which interleave with Go benchmark
lines in the raw logs; preserve those logs and account for that if importing
results into a benchmark parser.

`-cpu=1` sets GOMAXPROCS, **not CPU affinity**. It makes CPU cost comparisons
simpler but serializes Go execution across the two TCP endpoints. A follow-up
with `-cpu=2` or deployment-like concurrency is a separate experiment, not a
substitute for these numbers. Change `buffer=32KiB` in a profile selector to
`buffer=256KiB` or `buffer=1024KiB` to profile another buffer size.

## Measured results (2026-09-09)

Environment: Go 1.26.0 linux/amd64, AMD Ryzen 9 7950X (16 cores / 32 threads),
frequency boost enabled. Repository baseline HEAD was
`db4bb47c22d7daad83eca612d6ad416eb35c0e90` **plus existing uncommitted edits**;
this is not a clean-commit baseline. Full environment/status is in
`benchmark-results/environment.txt`. The project/artifacts filesystem is ext4;
`/tmp` is tmpfs on this host. The local temporary-file workload is therefore
warm-memory oriented, not a durable-disk throughput measurement.

### Historical production path, loopback TCP (hashing enabled)

Medians of three unprofiled 500 ms samples from `matrix.txt`:

| Operation | Median MB/s | Observed MB/s range | B/op | allocs/op |
| --- | ---: | ---: | ---: | ---: |
| SendFile | 586.35 | 578.10–625.50 | 67,448 | 36 |
| SendFile + atomic progress | 587.53 | 543.75–591.47 | 67,512 | 36 |
| ReceiveFile | 555.92 | 554.02–589.55 | 67,112–67,146 | 34 |

The separate profiled run measured 575.35 MB/s send and 576.80 MB/s receive.
These are not used as substitutes for the unprofiled medians.

### Synthetic buffer experiments

Median payload MB/s of three unprofiled samples; `atomic` means the synchronous
atomic-add callback, not a complete application progress reporter:

| Buffer | No hash, nil | No hash, atomic | SHA-256, nil | SHA-256, atomic | B/op no hash / hash | allocs/op no hash / hash |
| --- | ---: | ---: | ---: | ---: | ---: | ---: |
| 32 KiB | 17,362.18 | 17,177.23 | 1,862.85 | 1,919.46 | 32,792 / 32,976 | 2 / 5 |
| 256 KiB | 18,351.86 | 18,122.52 | 1,908.80 | 1,965.39 | 262,168 / 262,352 | 2 / 5 |
| 1 MiB | 10,138.91 | 9,932.96 | 2,023.49 | 2,013.17 | 1,048,600 / 1,048,784 | 2 / 5 |

### CPU-top evidence

Flat sample percentages from the saved `go tool pprof -top` reports:

| Profile | Total sampled CPU | Leading flat CPU entries |
| --- | ---: | --- |
| `synthetic-sha256.cpu` | 4.08 s | `crypto/internal/fips140/sha256.blockSHANI`: **67.89%** (2.77 s); `runtime.memmove`: **31.37%** (1.28 s) |
| `synthetic-copy.cpu` | 9.32 s | `runtime.memmove`: **97.42%** (9.08 s) |
| `server-tcp.cpu` | 10.13 s | `internal/runtime/syscall/linux.Syscall6`: **53.70%** (5.44 s); `crypto/internal/fips140/sha256.blockSHANI`: **44.03%** (4.46 s) |

The real profile includes both directions and both endpoints. It confirms
production `Server.handleSend` (30.40% cumulative) and `Server.handleReceive`
(19.94% cumulative) are exercised. Cumulative percentages overlap with their
callees and must not be added to flat percentages. Syscall CPU includes both
files and loopback sockets; this profile alone cannot attribute it to one or
establish network wait time. No benchmark uses `net.Pipe`.

## Conclusions and limitations

- SHA-256 is the largest isolated CPU cost in the synthetic hashing profile,
  using hardware SHA instructions on this host. Copy-only is genuinely copying,
  as its `memmove` profile shows. The no-hash experiment is an analytical
  baseline, **not a recommendation to disable checksums**.
- 1 MiB improves the hash-enabled median by about 9% over 32 KiB here, but costs
  roughly 1 MiB of allocation per transfer versus 32 KiB. It substantially hurts
  copy-only throughput. These small repeated samples do not establish an
  optimal buffer, nor justify changing production defaults. Allocation, cache
  locality, and hash/copy interaction all differ across cases.
- Atomic-callback overhead is not reliably resolved: some callback cases are
  faster than their nil counterparts, and sample ranges overlap. That is noise,
  not evidence that callbacks make transfers faster or that real UI progress is
  free. More repetitions and a realistic progress consumer would be needed.
- Synthetic data and destination fit in the host's last-level cache. This is
  not large-working-set DRAM bandwidth, disk bandwidth, or expected end-to-end
  throughput. Its hash-enabled case hashes once; the historical real transfer
  hashed at both endpoints. Synthetic no-hash results do not measure the new
  production no-hash protocol.
- The real TCP result includes production framing and files but uses loopback,
  warm temporary storage, one persistent connection, a single client, and
  GOMAXPROCS=1. It does not model remote latency, independent machines/CPUs,
  packet loss, storage durability, or concurrent transfers. Larger buffers were
  tested **only synthetically in this baseline**. Later production buffer-size
  experiments are recorded in [the remote results](benchmark-results.md); they
  also used TCP, not QUIC.
- `ReceiveStream` was excluded from the historical baseline: inspection then
  showed it read payload from `t.conn` after reading the header through `t.reader`, unlike `ReceiveFile`.
  Buffered read-ahead can make that path unsuitable for a trustworthy baseline
  without a production correctness change. No workaround or protocol edit was
  introduced. `SendStream` and other transports are also outside this baseline.
- Host frequency scaling and other machine activity were not controlled; there
  are only three short samples, with no statistical significance claim. CPU
  calibration can make a `-benchtime=3s` process/profile longer than three seconds.
- The smoke run, all repeated/profile runs, and the **benchmark-only** race smoke
  passed. The full ordinary suite and full race suite were deliberately not run.
  Remote RAM-disk and rsync comparisons were separate work, now recorded in
    [the TCP follow-up results](benchmark-results.md).

Artifacts are local generated output under `benchmark-results/`: raw smoke,
race-smoke, matrix and profile-run logs, environment metadata, three `.cpu`
profiles, three `*-top.txt` reports, and the two matching test binaries. No
production or ignore-file edits are needed to use these benchmarks.
