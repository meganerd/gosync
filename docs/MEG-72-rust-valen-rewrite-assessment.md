# MEG-72: Go, Rust, and Valen rewrite assessment

Date: 2026-10-15

## Decision

Do not rewrite gosync in Rust or Valen for performance today. Keep the Go
implementation and optimize measured bottlenecks. A Rust prototype is justified
only for a small, replaceable hot path after profiling shows that Go or a Go
dependency is the limiting factor. Valen is not suitable for the production
transfer engine yet.

This conclusion preserves gosync's CLI, wire protocol, deployment workflow, and
platform compatibility. It does not rule out Rust components or a future rewrite;
it requires evidence and compatibility tests before accepting that cost.

## Evidence from gosync

The repository already contains end-to-end measurements and CPU profiles:

- On Agrippa (Xeon E5-2697A v4), the original checksum-enabled TCP transfer
  achieved 248.53 MB/s median versus rsync's 360.22 MB/s. The receiver consumed
  much more CPU than the sender.
- The isolated checksum profile spent 96.38% of samples in Go's AVX2 SHA-256
  block routine and achieved about 330 MB/s. This is an algorithm/hardware limit,
  not evidence that garbage collection or Go dispatch dominated the transfer.
- Pipelining I/O and SHA-256 in Go improved end-to-end median throughput by 8.7%.
  Increasing each pipeline buffer from 32 KiB to 256 KiB added another 6.9%
  median in a later matrix. These gains came from architecture and tuning without
  changing language or protocol.
- Application SHA-256 is now optional and off by default. The no-hash path avoids
  the previously measured bottleneck while QUIC retains transport encryption and
  integrity.

Current local benchmarks on an AMD Ryzen 9 7950X (Go 1.25 module, run
2026-10-14, three 300 ms samples) provide an upper-bound sanity check:

| Path | Observed throughput | Allocation profile |
| --- | ---: | ---: |
| Synthetic copy, no hash, 32 KiB | 20.5-30.5 GB/s | 32,792 B, 2 allocs/op |
| Synthetic copy + SHA-256, 32 KiB | 1.92-2.22 GB/s | 32,976 B, 5 allocs/op |
| TCP loopback send, no hash | 1.78-1.98 GB/s | ~67 KiB, 30 allocs/op |
| TCP loopback receive, no hash | 1.30-1.46 GB/s | ~67 KiB, 30 allocs/op |

Command:

```sh
go test ./pkg/checksum ./pkg/transport ./pkg/server -run '^$' \
  -bench 'Benchmark(CopyNWithSHA256|TransferLoopSynthetic|ServerTransportTCP)$' \
  -benchmem -benchtime=300ms -count=3
```

The built, stripped-path Go executable is 11,638,794 bytes. The repository has
about 16,386 lines of Go including tests. Binary size and idle memory may be
reduced by Rust, but neither has been established as an operational constraint.

## Fresh CPU and allocation profiles

Profiles were captured on 2026-10-15 on the same Ryzen 9 7950X. Each selected
benchmark ran for five seconds with CPU and heap profiling enabled. These are
local hot-path diagnostics, not remote throughput claims.

```sh
go test ./pkg/server -run '^$' \
  -bench '^BenchmarkServerTransportTCP/SendFile$' -benchtime=5s -count=1 \
  -cpuprofile=server-send.cpu -memprofile=server-send.mem -o server.test

go test ./pkg/transport -run '^$' \
  -bench '^BenchmarkTransferLoopSynthetic/buffer=32KiB/SHA256=true/ProgressAtomic=false$' \
  -benchtime=5s -count=1 -cpuprofile=sha256.cpu -memprofile=sha256.mem \
  -o transport.test

go test ./pkg/transport -run '^$' \
  -bench '^BenchmarkTransferLoopSynthetic/buffer=32KiB/SHA256=false/ProgressAtomic=false$' \
  -benchtime=5s -count=1 -cpuprofile=copy.cpu -memprofile=copy.mem \
  -o transport-copy.test
```

### Production no-hash TCP send

The deployed client/server path transferred 8 MiB per operation at 1,847.75
MB/s, allocating 67,010 bytes in 30 allocations. Of 9.22 CPU-seconds sampled
across both loopback endpoints, `internal/runtime/syscall/linux.Syscall6`
accounted for 8.99 seconds (**97.51%**). Cumulative syscall write time was 5.04
seconds and read time 2.87 seconds. `checksum.CopyNBuffer` was on the stack for
85.90% of samples but had only 0.22% flat CPU: it is where the I/O waits occur,
not a compute-heavy Go loop.

The allocation profile attributed 84.59 MiB across the run to
`checksum.CopyNBuffer`, consistent with allocating one 32 KiB copy buffer at
each endpoint per operation. The benchmark's final `os.ReadFile` verification
accounted for another 24.02 MiB and is outside the timed transfer. The timed
allocation rate is about 0.8% of payload bytes. No garbage-collector function
appeared as a material CPU consumer.

Finding: current local TCP throughput is syscall/I/O-bound. A Rust rewrite that
performs the same 32 KiB reads and writes would retain the dominant cost.

### Checksum-enabled copy

The 32 KiB synthetic checksum path achieved 2,283.59 MB/s. Hardware SHA-256
(`crypto/internal/fips140/sha256.blockSHANI`) consumed 82.17% of CPU and
`runtime.memmove` consumed 16.53%; together they account for 98.70%.

Finding: checksum-on throughput is bounded by the digest and necessary memory
movement. Language/runtime overhead is below the resolution needed to justify a
rewrite. On Agrippa, where AVX2 SHA-256 is much slower than SHA-NI on this host,
the digest ceiling is more severe.

### Copy-only memory loop

The copy-only synthetic path achieved 28,934.50 MB/s and spent 97.15% of CPU in
`runtime.memmove`. It allocated one 32 KiB transfer buffer per operation (32,792
bytes and two allocations reported by the benchmark).

Finding: Go dispatch, progress reporting, and garbage collection are not visible
bottlenecks in the unencrypted copy loop. This result is a memory benchmark, not
a network rate, but it rules out a large generic language-overhead claim.

### Ranked remediations

| Priority | Remediation | Expected effect | Validation and tradeoff |
| --- | --- | --- | --- |
| 1 | Benchmark 256 KiB and 1 MiB buffers on the actual TCP/QUIC links and storage; consider making 256 KiB the default only if the matrix holds. | Fewer read/write syscalls; prior checksum-enabled remote work measured +6.9% at 256 KiB. | Measure throughput, CPU/GiB, and RSS at supported worker/connection counts. Four checksum buffers increase per-copy memory from 128 KiB to 1 MiB at 256 KiB. |
| 2 | Profile production QUIC and multi-connection QUIC separately. | Identifies TLS/AEAD, packetization, congestion, commit-readback, or storage limits hidden by TCP profiling. | Use end-to-end target hardware; do not infer QUIC behavior from TCP loopback. |
| 3 | Keep application SHA-256 off where QUIC/TLS integrity satisfies the threat model; when end-to-end file digests are required, evaluate a negotiated faster digest. | Removes or reduces the measured digest ceiling. | Any new digest is a wire-protocol/security decision, must be negotiated, and must preserve SHA-256 compatibility. Do not weaken integrity silently. |
| 4 | Reuse copy buffers with a bounded pool only if allocation/GC profiles on high-concurrency workloads show pressure. | Saves roughly one buffer allocation per endpoint per transfer. | Low expected throughput gain in the current profile; cap retained memory and benchmark contention. |
| 5 | Experiment with Linux-specific `sendfile`/`splice` only for eligible no-hash, no-progress transformations. | May reduce copies and userspace syscall overhead. | Complicates portability and progress semantics; cannot directly serve checksum, compression, QUIC/TLS, or arbitrary stream paths. Keep a portable fallback. |
| 6 | Build a narrow Rust hot-path prototype only after one of the above leaves a measured Go-specific limit. | Could improve footprint or expose lower-level platform APIs. | Require at least 20% target-system improvement and protocol-equivalence/security tests before adoption. |

The immediate experiment is therefore a production TCP/QUIC buffer-size matrix,
not a rewrite. The existing `--buffer-size` control makes this reversible and
requires no compatibility break.

## What Rust could improve

Rust can plausibly reduce runtime footprint, eliminate garbage-collector pauses,
and offer tighter control over buffer ownership, syscalls, zero-copy APIs, and
CPU-specific implementations. Its ecosystem also offers mature async I/O and
QUIC implementations. Those are potential advantages, not measured gosync
speedups.

For bulk file transfer, disk, network, encryption, hashing, protocol round trips,
and buffer topology normally dominate. Reimplementing the same algorithm in Rust
will not remove a SHA-256 throughput ceiling or a slow storage/network link. Go's
current hot paths have low allocation counts, and the measured no-hash loopback
path already exceeds common LAN throughput.

A rewrite would also recreate roughly 16 KLOC of implementation and tests,
including TCP, SSH, QUIC, TLS verification/pinning, parallel ranged transfer,
resume state, deployment, bandwidth limiting, path validation, and cross-platform
install behavior. The highest risks are silent wire incompatibility, security
regressions, and divergence during a long dual-maintenance period.

## Valen assessment

Valen is an interesting future option, but its official repository currently
describes it as a prototype "barely past the proof-of-concept stage" and says
0.1 will be released when stable. Its compiler is implemented in Rust and its
design discusses Rust interoperability, but that does not yet establish stable,
general Cargo-crate compatibility suitable for gosync's QUIC/TLS stack.

Using Valen now would combine two migrations: rewriting gosync and adopting a
pre-0.1 language/toolchain. It would add compiler, debugger, packaging, security
response, dependency-interop, and contributor-availability risk. If direct Rust
ecosystem access is the deciding requirement, Rust itself is the lower-risk
choice until Valen publishes and demonstrates a stable interop contract.

Source: [official Valen repository](https://github.com/valen-lang/Valen).

## Recommended path

1. Establish the required outcomes before changing language: target throughput,
   CPU per GiB, peak RSS, startup time, executable size, and target hardware.
2. Benchmark the current default no-hash TCP and QUIC paths end to end on the
   actual 1/10/25 GbE links and storage. Existing remote measurements cover
   checksum-enabled TCP and should not be relabeled.
3. Profile any failing scenario. Prefer changes such as connection fan-out,
   buffer/socket tuning, syscall reduction, direct-I/O experiments, or a
   negotiated faster integrity algorithm when the profile supports them.
4. If a Go ceiling remains, build a narrow Rust spike implementing only the
   identified hot path. Compare it with the same fixture, protocol semantics,
   verification, and resource measurements. Keep the process boundary or FFI
   boundary small and replaceable.
5. Consider a full Rust rewrite only if the spike demonstrates a material gain
   (suggested gate: at least 20% throughput or CPU improvement on target systems)
   that cannot be obtained in Go, and the project accepts the compatibility and
   maintenance cost explicitly.
6. Revisit Valen after a stable release provides documented Rust-crate interop,
   supported platforms, reproducible builds, debugging/profiling, dependency
   management, and a security/update policy.

## Compatibility gate for any prototype

Any Rust or Valen implementation must remain opt-in until it passes the existing
Go implementation's protocol and behavior suite. At minimum, test old/new client
and server combinations where compatibility is promised; QUIC/TCP checksum modes;
TLS verification and pinning; truncated/corrupt transfers; unsafe paths; resume;
ranged commits; deployment cleanup; Linux/macOS/Windows packaging; and equivalent
telemetry. A faster implementation that weakens integrity, authentication, or
failure handling is not an acceptable result.
