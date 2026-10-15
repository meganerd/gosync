# Bounded copy-buffer reuse under concurrency

Measured 2026-10-15 on an AMD Ryzen 9 7950X (32 logical CPUs), Go 1.25.
`BenchmarkCopyNConcurrent` runs independent 1 MiB transfers at 1, 8, 32, and
128 workers. It deliberately emphasizes transfer churn: the numbers are a
copy-loop ceiling, not a network throughput claim.

## Decision

The baseline profiles showed material allocation/GC pressure, so a bounded
prototype was warranted and retained. Default-sized plain buffers and
four-buffer SHA-256 slabs now use separate nonblocking pools. Custom configured
sizes keep the prior allocate-per-transfer behavior.

The maximum retained memory is 8 MiB:

- 128 plain 32 KiB buffers: 4 MiB
- 32 SHA-256 128 KiB slabs: 4 MiB

Returning a buffer clears its complete capacity before it becomes reusable, so
bytes from one transfer cannot be exposed to the next. A full pool drops the
buffer for garbage collection; acquisition never waits for pool capacity.

## Results

Median of three 300 ms samples:

| Mode | Workers | Before | After | Before bytes/op | After bytes/op |
| --- | ---: | ---: | ---: | ---: | ---: |
| Plain | 1 | 61.4 GB/s | 73.4 GB/s | 32,816 | 48 |
| Plain | 8 | 213.9 GB/s | 465.0 GB/s | 32,816 | 48 |
| Plain | 32 | 222.4 GB/s | 769.5 GB/s | 32,816 | 48 |
| Plain | 128 | 194.6 GB/s | 764.4 GB/s | 32,817 | 49 |
| SHA-256 | 1 | 2.17 GB/s | 2.36 GB/s | 131,873 | 800 |
| SHA-256 | 8 | 14.4 GB/s | 19.1 GB/s | 131,875 | 800 |
| SHA-256 | 32 | 26.1 GB/s | 66.5 GB/s | 131,875 | 800 |
| SHA-256 | 128 | 39.6 GB/s | 64.0 GB/s | 131,873 | 1,552 |

At 128 workers the baseline plain CPU profile spent 6.33% flat in
`runtime.memclrNoHeapPointers`, 3.97% in `runtime.(*lfstack).pop`, plus further
GC scanning, sweeping, locking, and reclamation. The SHA-256 profile remained
digest-bound (80.73% flat in hardware SHA), but allocation clearing was still
2.26%. The throughput changes above show that the allocator/GC cost was
material in this high-churn workload. The modest residual SHA allocation is
from the worker/channels/hash state, not its 128 KiB buffer slab.

The 128-worker result also exercises pool contention. Throughput improved rather
than regressed, and nonblocking get/put means pool saturation cannot stall a
transfer. Real network transfers will see smaller gains because I/O dominates.

## Reproduce

```sh
go test ./pkg/checksum -run '^$' -bench '^BenchmarkCopyNConcurrent$' \
  -benchmem -benchtime=300ms -count=3

go test ./pkg/checksum -run '^$' \
  -bench '^BenchmarkCopyNConcurrent/SHA256=false/concurrency=128$' \
  -benchtime=5s -count=1 -cpuprofile=plain.cpu -memprofile=plain.mem \
  -o checksum.test
go tool pprof -top checksum.test plain.cpu
```

Correctness, the exact retention cap, clearing before reuse, mismatched-buffer
rejection, and the race detector are covered by the checksum package tests.
