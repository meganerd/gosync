# MEG-78: default copy-buffer evaluation

## Recommendation

Retain the compatible **32 KiB** default. Keep `--buffer-size` as the opt-in
control for operators who have benchmarked their own workload. Do not change
the default from the available evidence.

The larger buffers improve a single large, warm-cache transfer by 2.6–3.8% on
the two measured QUIC targets and 6.9–7.8% on the older checksum-enabled TCP
matrix. Those gains are modest relative to the memory multiplication at useful
concurrency. 256 KiB is the only plausible future default candidate: 1 MiB was
not consistently faster and uses four times as much payload-buffer memory.

## Evidence reviewed

All network trials transferred and independently verified the same
6,140,975,104-byte ISO to RAM-backed destinations. Rates are medians of three
rotated-order trials. QUIC retained TLS and certificate verification. The 2026
QUIC matrix explicitly disabled the optional application SHA-256; the older TCP
size matrix explicitly enabled it, so absolute rates must not be compared
between the two matrices.

| Target and transport | 32 KiB | 256 KiB | 1 MiB | 256 KiB delta | 1 MiB delta |
| --- | ---: | ---: | ---: | ---: | ---: |
| ai01-1010 QUIC | 841.93 MiB/s | 873.81 MiB/s | 873.44 MiB/s | +3.8% | +3.7% |
| agrippa-1010 QUIC | 222.36 MiB/s | 228.15 MiB/s | 230.75 MiB/s | +2.6% | +3.8% |
| agrippa-1010 TCP + SHA-256 | 256.33 MiB/s (268.78 MB/s) | 274.10 MiB/s (287.42 MB/s) | 276.44 MiB/s (289.87 MB/s) | +6.9% | +7.8% |

On ai01, the uninstrumented QUIC trials also retained process CPU and receiver
peak-RSS measurements. CPU/GiB divides median process CPU seconds by the 5.72 GiB
payload; it is not whole-host CPU.

| Buffer | Sender CPU/GiB | Receiver CPU/GiB | Receiver peak RSS |
| --- | ---: | ---: | ---: |
| 32 KiB | 2.81 s | 1.53 s | 17.1 MiB |
| 256 KiB | 2.65 s | 1.47 s | 17.4 MiB |
| 1 MiB | 2.61 s | 1.48 s | 17.9 MiB |

The observed RSS increase is small at one active copy, as expected, but the
buffer allocation itself scales deterministically:

| Mode | 32 KiB | 256 KiB | 1 MiB |
| --- | ---: | ---: | ---: |
| checksum off, per active copy and endpoint | 32 KiB | 256 KiB | 1 MiB |
| checksum on, per active copy and endpoint | 128 KiB | 1 MiB | 4 MiB |

Thus 16 checksum-enabled active copies require 2 MiB, 16 MiB, or 64 MiB of
payload buffers per endpoint, before protocol, socket, goroutine, filesystem,
and page-cache memory. QUIC's default four-way fan-out can further multiply
active ranged copies for large files.

## Concurrency scaling

The retained transport harness used a fixed 8 GiB in-memory payload, 1 MiB
buffers, TLS, and one, two, four, or eight independent sockets. It is a transport
ceiling rather than the gosync file protocol, but it isolates the scaling axis.

| Sockets | QUIC MiB/s | QUIC scaling | TLS/TCP MiB/s | TCP scaling |
| ---: | ---: | ---: | ---: | ---: |
| 1 | 841.21 | 1.00x | 1942.56 | 1.00x |
| 2 | 1545.95 | 1.84x | 2410.09 | 1.24x |
| 4 | 1952.28 | 2.32x | 2713.06 | 1.40x |
| 8 | 2180.57 | 2.59x | 2883.97 | 1.48x |

Follow-up topology controls showed that sender socket count, not additional
streams or connections sharing one socket, carries the QUIC gain. This supports
the existing connection fan-out work but does not show that larger copy buffers
improve concurrent gosync transfers. Buffer memory grows linearly while
per-socket throughput shows sharply diminishing returns.

## Decision rationale and limits

- 256 KiB is near the 1 MiB result on every measured path, but its benefit is
  only 2.6–6.9% and varies with host and checksum mode.
- 1 MiB has no defensible default case: on ai01 QUIC it was fractionally slower
  than 256 KiB; on Agrippa it bought only another 1.1–1.4%.
- The matrix covers one large sequential file, warm source cache, RAM-backed
  destinations, one direction, and three observations per cell. It does not
  establish benefits for small files, durable storage, constrained-memory
  systems, WANs, or many simultaneous files.
- Sender peak RSS was not retained in the QUIC runner. Receiver RSS and exact
  buffer-allocation bounds are reported rather than inventing a sender value.
- The retained TCP all-size matrix used checksums; the later checksum-off matrix
  measured TCP only at 32 KiB (2302.70 MiB/s on ai01, 1153.33 MiB/s on Agrippa).
  A checksum-off TCP all-size rerun could refine tuning guidance, but is not
  necessary to reject a compatible-default change: the existing favorable TCP
  result still does not outweigh the workload and concurrency limitations.

No production default or protocol behavior was changed.

## Reproducibility and provenance

Primary retained evidence:

- `benchmark-results/quic-matrix/requested-experiments-summary.json`
- `benchmark-results/quic-matrix/REQUESTED-EXPERIMENTS.md`
- `benchmark-results/quic-matrix/requested-ai01-diagnostics.md`
- `benchmark-results/quic-matrix/requested-agrippa-diagnostics.md`
- `benchmark-results/quic-matrix/connscale-findings.md`
- `benchmark-results/quic-matrix/connscale-ai01-20260911T070833/summary.json`
- `benchmark-results/buffer-sizes/round{1,2,3}-{32,256,1024}/`
- `docs/benchmark-results.md`

The QUIC matrix reports 36/36 independently verified transfers across the two
targets. The TCP buffer-size matrix reports 9/9 gosync transfers verified for
length and SHA-256. The connection-scaling harness reports 24/24 byte-exact
trials. Raw schedules, commands, environment, binary provenance, per-trial
records, cleanup evidence, and preserved artifacts remain under the paths above.
