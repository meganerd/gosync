# MEG-80: Linux zero-copy prototype

## Scope

The prototype uses Go's `io.CopyN` regular-file-to-`*net.TCPConn` fast path,
which is backed by Linux `sendfile(2)`. It is disabled by default and available
only through `transport.Config.ZeroCopy` for benchmarking. The CLI does not
enable it.

Eligibility is deliberately narrow: Linux, a regular `*os.File` source and
`*net.TCPConn` destination, application SHA-256 disabled, and no progress
callback. Every other platform or operand shape uses the existing bounded
buffered copy. Progress-enabled transfers also use that fallback, preserving
incremental progress. Existing size-first wire framing and acknowledgement
validation are unchanged.

## Benchmark

The end-to-end benchmark transferred an 8 MiB file over loopback into the real
gosync TCP server. The recorded run used `SendFile` for the prototype before
the opt-in gate was added; the durable benchmark name is now
`SendFileZeroCopy`. `SendFileProgressAtomic` sets a callback and exercises the
portable fallback even when zero-copy is requested.
Five one-second samples were collected on an AMD Ryzen 9 7950X, Linux amd64,
Go 1.25.

| Path | Throughput range | Approx. median | Heap bytes/op |
| --- | ---: | ---: | ---: |
| Linux sendfile prototype | 1.60-1.91 GB/s | 1.84 GB/s | 1.75 KiB |
| Buffered + progress fallback | 1.80-2.01 GB/s | 1.91 GB/s | 1.50 KiB |

The prototype did not improve loopback throughput or allocations; its median
throughput was about 3.5% lower in this sample. Scheduler, page-cache,
NIC, and remote-storage behavior can differ substantially, so this is not a
production recommendation.

## Recommendation

Keep the path disabled by default. Before exposing it in the CLI, benchmark on
a physical network with representative large files and collect CPU time,
throughput, and retransmit data. If enabled later, either retain the current
progress fallback or add chunked sendfile calls and verify that they preserve
deadline/error behavior without erasing the CPU benefit.
