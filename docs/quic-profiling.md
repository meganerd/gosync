# QUIC hot-path profiling

`BenchmarkQUICSend` drives the production QUIC client and server together over
loopback with a deterministic 64 MiB + 4097 byte file. Its four cases compare
one persistent QUIC connection with four fresh sender sockets/connections and
compare checksum-off with checksum-on. TLS authentication, QUIC AEAD,
packetization, UDP syscalls, staging-file writes, commit `fsync` and rename are
real. Checksum-on additionally hashes every range at both ends, hashes the whole
source concurrently, and reads and hashes the whole staged file at commit.

Connection setup is outside the single-connection timer. Fan-out data
connections are intentionally created and destroyed per transfer because that
is the production behavior. Server and client execute in the same process, so
profiles contain both endpoints. The benchmark is a component profile, not an
end-to-end WAN throughput claim: loopback removes NIC, routing, loss and RTT,
and a warm local filesystem does not represent durable storage.

## Reproduce

Run on an otherwise idle Linux host from the repository root. Keep the test
binary with its profiles so symbolization always matches. `-cpu=1` limits Go
scheduling but does not pin the process; use `taskset` separately when CPU
affinity matters.

```sh
mkdir -p benchmark-results/quic-profile
export GOTOOLCHAIN=local
export GOPROXY=off
export GOCACHE=/tmp/gosync-go-cache

# Compile and exercise every cell once.
go test ./pkg/transport -run '^$' -bench '^BenchmarkQUICSend$' \
  -benchtime=1x -count=1 -benchmem -timeout=120s

# Three unprofiled samples. Use these for throughput and allocation comparisons.
go test ./pkg/transport -run '^$' -bench '^BenchmarkQUICSend$' \
  -benchtime=3x -count=3 -benchmem -cpu=1 -timeout=10m \
  > benchmark-results/quic-profile/matrix.txt 2>&1

# One profile per cell; a combined profile obscures topology attribution.
for connections in 1 4; do
  for checksum in false true; do
    stem="c${connections}-checksum-${checksum}"
    go test ./pkg/transport -run '^$' \
      -bench "^BenchmarkQUICSend$/connections=${connections}/checksum=${checksum}$" \
      -benchtime=5x -count=1 -benchmem -cpu=1 -timeout=10m \
      -cpuprofile="benchmark-results/quic-profile/${stem}.cpu" \
      -memprofile="benchmark-results/quic-profile/${stem}.alloc" \
      -o="benchmark-results/quic-profile/${stem}.test" \
      > "benchmark-results/quic-profile/${stem}.txt" 2>&1
    go tool pprof -top -nodecount=40 \
      "benchmark-results/quic-profile/${stem}.test" \
      "benchmark-results/quic-profile/${stem}.cpu" \
      > "benchmark-results/quic-profile/${stem}-cpu-top.txt"
    go tool pprof -top -alloc_space -nodecount=40 \
      "benchmark-results/quic-profile/${stem}.test" \
      "benchmark-results/quic-profile/${stem}.alloc" \
      > "benchmark-results/quic-profile/${stem}-alloc-top.txt"
  done
done

go version > benchmark-results/quic-profile/environment.txt
uname -a >> benchmark-results/quic-profile/environment.txt
lscpu >> benchmark-results/quic-profile/environment.txt
df -T . "${TMPDIR:-/tmp}" >> benchmark-results/quic-profile/environment.txt
git rev-parse HEAD >> benchmark-results/quic-profile/environment.txt
git --no-optional-locks status --short >> benchmark-results/quic-profile/environment.txt
```

The benchmark destination is under `b.TempDir()`, so `TMPDIR` selects the
storage under test. Repeat the matrix with a tmpfs `TMPDIR` and with `TMPDIR` on
the deployment filesystem. The delta includes filesystem and storage effects;
do not label it as device time without block-level evidence. `fsync` semantics
on tmpfs differ from persistent filesystems.

For syscall counts and time, profile one exact benchmark cell with `strace`.
Do not use this run for throughput because tracing changes timing materially:

```sh
strace -f -c -o benchmark-results/quic-profile/c4-nohash-strace.txt \
  go test ./pkg/transport -run '^$' \
  -bench '^BenchmarkQUICSend$/connections=4/checksum=false$' \
  -benchtime=1x -count=1 -cpu=1 -timeout=3m
```

## Attribution

Use call graphs (`go tool pprof -http=:0` or `-top -cum`) before assigning a
sample. Flat samples alone frequently land in generic runtime or syscall frames.

| Cost | Evidence in CPU/call graph | Controlled comparison |
| --- | --- | --- |
| TLS handshake | `crypto/tls`, certificate and signature functions called below QUIC dial/accept | Primarily fan-out minus single connection; fan-out creates four data handshakes per operation |
| QUIC AEAD | `crypto/aes`, `crypto/cipher`, `chacha20poly1305`, QUIC packet protect/unprotect frames | Present in every cell; cannot be disabled without ceasing to profile production QUIC |
| Packetization | `quic-go` packet packing, framing, ACK, send/receive loops | Four versus one connection at the same checksum setting |
| UDP syscall | `net.(*UDPConn)`, `internal/poll`, `runtime` syscall frames; corroborate with `strace -c` | Four versus one connection; profile includes both sender and receiver |
| Range checksum | SHA-256 below range send/receive copy helpers | Checksum-on minus checksum-off, noting whole-file work below |
| Storage | `os.File` read/write, `WriteAt`, `fsync`, rename and their syscall descendants | Persistent-filesystem run minus tmpfs run; includes page cache/filesystem effects |
| Commit readback | SHA-256 and file reads below `handleCommit` / commit verification | Only checksum-on fan-out does receiver whole-file readback; inspect the call graph rather than treating the full checksum delta as readback |

The checksum comparison is intentionally an upper bound on checksum cost: it
contains per-range hashing, the sender's whole-file hash, and receiver commit
readback/hash. To isolate commit readback precisely without changing production
semantics, filter the checksum-on call graph by the server commit handler. Do
not subtract unrelated flat SHA totals: sender range hashing, receiver range
hashing, source hashing, and commit hashing overlap in time.

Allocation profiles default to sampled `alloc_space`. Benchmark `B/op` and
`allocs/op` are authoritative whole-operation totals; use `pprof -alloc_space`
to find volume and `-inuse_space` only for retained-memory questions. Because
fan-out opens connections per operation, its allocations include handshake and
teardown by design.

## Interpretation guardrails

- Compare unprofiled medians, not profiled throughput.
- Record Go, quic-go, kernel, CPU, filesystem, tree status, payload and exact
  command with every result.
- Treat TLS, AEAD, packetization and UDP syscall percentages as combined client
  plus server cost on loopback.
- CPU profiles measure on-CPU samples, not I/O wait. Use wall time, syscall
  tracing and host storage telemetry to reason about waiting.
- A faster tmpfs run does not prove the same gain on a remote receiver, and a
  loopback socket-count result does not establish WAN scaling.

## Initial profile snapshot (2026-10-15)

This short validation run used Go 1.26.0, linux/amd64, an AMD Ryzen 9 7950X,
`/tmp` on tmpfs, repository commit `0c93edb12e54c4a09550a801ade8ec6614bea56a`
plus concurrent uncommitted work, and three timed transfers per profiled cell.
These are profiled single samples, so they validate attribution and expose hot
paths; they are not stable performance baselines.

| Connections | Checksum | Profiled MB/s | B/op | allocs/op |
| ---: | :---: | ---: | ---: | ---: |
| 1 | off | 350.65 | 7,301,429 | 217,785 |
| 4 | off | 330.66 | 10,833,517 | 234,544 |
| 1 | on | 284.65 | 7,277,706 | 217,760 |
| 4 | on | 188.12 | 12,026,322 | 237,164 |

The CPU profiles separate the major costs cleanly:

- Checksum-off single connection spent 57.1% of samples in the generic Linux
  syscall frame; AES-GCM encrypt/decrypt accounted for 4.4% combined. QUIC
  send-queue, packet encryption/unpacking and UDP read paths were visible in
  cumulative stacks.
- Checksum-off fan-out spent 41.0% in the syscall frame and 8.0% combined in
  AES-GCM encrypt/decrypt. Packet packing, connection receive paths, and
  independent send queues were prominent. This confirms the profile exercises
  packetization and AEAD rather than only file copying.
- Checksum-on introduced SHA-256 as a first-order cost: 14.7% for one
  connection and 25.3% for four. In the four-connection call graph, checksum
  copy work was 22.2% cumulative, range receive handling 6.3%, range sending
  5.7%, `pread` 8.9%, and receiver commit handling 1.9% cumulative.
- Generic syscall samples combine UDP and file operations. The call graph
  resolves substantial UDP work (`sendmsg` 13.9% cumulative and `recvmmsg`
  10.8% in four-connection checksum-on), while `pread` and `pwrite` expose
  source/readback and staged-file writes. A `strace -c` run is still required
  for syscall counts; CPU sampling cannot measure blocked storage time.

Fan-out did not improve this `GOMAXPROCS=1`, loopback, profiled tmpfs run. That
is expected to differ from the earlier unprofiled multi-core topology tests:
the single-P scheduler serializes both endpoints and profiling overhead is
large. The result is useful precisely as a CPU-cost view, not as evidence
against fan-out throughput scaling. The allocation totals show fan-out costs
roughly 3.5–4.7 MiB/op more in this sample, consistent with per-transfer data
connection handshakes, packet state, and teardown. Use the unprofiled matrix
before making performance or connection-count decisions.
