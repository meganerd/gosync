# Bounded transfer benchmark

`scripts/benchmark-transfer.py` compares the deployed **TCP server path** of gosync with a full-file rsync transfer over SSH. It uses Python 3 standard-library modules only; it does not build binaries, mount filesystems, use `-deploy`, or stop any pre-existing gosync service.

## Transport scope

The currently inspected runner invokes `-transport server` (TCP), starts `serve`
with its TCP default, and discovers/probes readiness using `/proc/net/tcp` and
`/proc/net/tcp6`. It does **not** currently expose `--transport quic`. Do not infer
runner QUIC support from the gosync CLI's QUIC default. If the runner is updated,
confirm its actual CLI, both endpoint commands, and UDP readiness handling before
using or documenting a QUIC benchmark option.

The production gosync receiver itself now supports `serve --transport tcp|quic`
(default TCP); transfer commands default to QUIC. `-deploy` honors `quic`, `tcp`,
or `server` (TCP), using SSH for bootstrap/cleanup, and rejects `ssh` or unknown
transports. There is no forced-TCP deployment override. That CLI change does not
change this runner's explicit TCP selection.

All earlier recorded gosync network measurements used **TCP**, not QUIC. The new
QUIC receiver and shared asynchronous buffer pipeline do not imply a performance
improvement. A separate full-ISO deployment was validated over UDP; see
[validation details](benchmark-results.md#production-quic-deployment-validation). QUIC also changed
wire format from the old experimental prototype, so update both binaries.
Pre-CAPS size-first/base64 TCP SHA-256 peers remain compatible with
`-checksum=true`; older path-first peers require both ends to be upgraded. The
new default no-hash mode requires an updated receiver. Current QUIC TLS uses ALPN
`gosync`, encrypts data, and authenticates the receiver via system PKI or an exact
leaf pin; deployment fetches and pins the public leaf over host-key-verified SSH.
TLS and pinning are unchanged by checksum mode. There is no client authentication;
restrict receiver access. See [security configuration](configuration.md#quic-security-and-compatibility).

## Checksum scope and historical results

The runner explicitly passes `-checksum` (equivalent to `-checksum=true`), so it
continues to measure application SHA-256 at **both endpoints**, using the current
size-first/base64 `SEND`/`RECEIVE` protocol with strict digest/size acknowledgements
and receive framing/trailers.
It does not expose a checksum-off option. External SHA-256 verification outside
the timer is separate from this in-transfer hashing.

For direct CLI transfers, `-checksum=false` is now the default for QUIC/TCP/server
and `-deploy`. It disables application hashing at both endpoints. `Connect` must
first complete a separate `CAPS` exchange with exactly `OK CAPS NOHASH`, then
uses `SEND-NOHASH` / `RECEIVE-NOHASH` with size framing and acknowledgements but
no digest. Unsupported receivers fail before file data. Upgrade the receiver,
or use `-checksum` only with immediate pre-CAPS size-first/base64 SHA-256 peers
(`SEND <size> <base64path>` / `RECEIVE`). This is not compatibility with arbitrary
older versions or the historical HEAD receiver's `SEND <literal-path> <size>`.
Those older path-first peers must **upgrade both ends**; no protocol auto-detection
or compatibility is promised. Size-first/base64 framing predates the checksum-mode
change; it was not introduced by it. There is no fallback. The receiver chooses the mode per request, not via a global
`serve` checksum flag. See [explicit on/off examples and migration](configuration.md#application-checksum-mode-and-compatibility).

**Historical gosync network runs always hashed, even if the recorded flag was
false.** Do not relabel them as genuine no-hash results or infer a performance
benefit from this change. Synthetic copy-only results are not end-to-end no-hash
measurements. TCP remains unencrypted and has no application digest in off mode;
QUIC TLS remains enabled in either mode. Standalone SSH has separate transport
integrity and now rejects `-checksum=true` rather than silently ignoring it.

## Run

From the repository root, with the intended local gosync binary already built:

```sh
python3 scripts/benchmark-transfer.py \
  --host agrippa-1010 \
  --source /home/gbjohnso/CD_Images/Win10_22H2_English_x64v1.iso \
  --destination /mnt/gosync-bench \
  --gosync ./gosync \
  --output ./benchmark-results/transfer \
  --runs 3 --warmups 1 --timeout 120
```

The expected ISO size is **6,140,975,104 bytes**. The runner measures the actual source size; it is reusable with other nonempty regular files and does not hardcode this ISO. Do not modify the source during a run.

Defaults are three measured rounds, one warmup round, and 120 seconds per command/hash. Each round transfers the file once with **each** tool: the defaults perform eight transfers. `--warmups 0` is supported. Increase `--timeout` if a full transfer or hash needs more time. Timeouts and verification failures fail the run rather than producing successful-looking partial summaries.

### Optional TCP buffer-size experiment

Add `--buffer-size 262144` to the command above to use 256 KiB buffers. The value must be an integer number of bytes in the inclusive range **4096..4194304** (4 KiB..4 MiB). The runner passes it to both the local gosync transfer command and the staged receiver's `gosync serve` command through the supervisor configuration; it never passes it to rsync. The selected value is recorded in the report's `arguments.buffer_size` field.

By default the option is **omitted**, not set to a numeric value: neither gosync endpoint receives a buffer-size flag, preserving compatibility with older experiment binaries that do not recognize it. Those older binaries use their existing **32 KiB** buffers. Omitted runs record `null`; the binary determines its own default. Explicit values require a binary supporting the production `--buffer-size` flag on both commands (the runner stages the same local binary remotely).

Because this runner enables checksums, budget **four buffers** per active copy: `4 * buffer_size` bytes, or 1 MiB at 262144 bytes and 16 MiB at the maximum, before other allocations. Direct no-hash transfers instead use **one buffer** per active copy (32 KiB by default, up to 4 MiB); this runner does not measure that mode. Account for sender and receiver memory as well as file cache/tmpfs usage. The production `--buffer-size` flag supports TCP/server and QUIC (not standalone SSH), but this runner's experiment remains **TCP-only**, not a QUIC benchmark or evidence about QUIC tuning.

### Prerequisites and capacity

- Local Unix/Python **3.8+**, executable gosync, OpenSSH `ssh`/`scp`, and rsync supporting `--protect-args`.
- Remote Linux/Python **3.8+**, rsync, readable `/proc` process/socket information, and a writable, executable filesystem under the SSH user's HOME for binary staging. The local binary must be compatible with the remote OS/architecture.
- SSH authentication and host-key trust must already work with `BatchMode=yes`. The runner never accepts unknown keys automatically or prompts for credentials.
- The requested destination must already exist and be writable. Each capacity check requires source size **plus 64 MiB** free there. HOME needs binary size plus 16 MiB. Free destination capacity is checked again before every transfer.
- The local source must be readable, regular, and nonempty. It is streamed without copying or allocating another ISO locally; local filesystem/free-space metadata is recorded, but source-sized local free space is not required. Leave space in `--output` for JSON and captured logs.
- For the stated setup, `/mnt/gosync-bench` is an existing **10 GiB tmpfs**, mounted `noswap,nodev,nosuid,noexec`, owned by UID 1000. The runner records mount information but does not enforce those particular mount options or change them. Authenticate as an account that can write there.

**tmpfs is volatile and can cause memory pressure or OOM.** Filesystem free-space checks are not a reservation or a guarantee of available physical memory, especially with `noswap`, concurrent workloads, or cgroup limits. Source cache, receiver data, and process memory all compete for RAM. The runner neither drops caches nor modifies mounts. Allow only one benchmark invocation at a time on this 10 GiB target.

## Method

1. Record local tool versions and environment, then stream SHA-256 over the source once to warm the source cache and establish the expected digest. This hash has its own deadline.
2. Use a read-only SSH `python3 -` helper to resolve remote HOME and the destination. Before preparation, record the ownership token and deterministic paths in the local report: `HOME/gosync-bench-<token>-bin` for the binary/helpers/logs and `DESTINATION/gosync-bench-<token>-data` for data. Create those directories exclusively with mode `0700`; existing paths are never adopted or given ownership markers. Copy only the binary with scp. No executable is placed on the noexec target.
3. Start **one** remote receiver for the entire run, using detached `subprocess.Popen` sessions and redirected logs. `gosync serve --listen 0.0.0.0:0 --base <owned-data-directory>` asks the kernel for an ephemeral port. Readiness is bounded to 15 seconds, additionally subject to the command timeout: the helper finds a listening socket owned by the recorded receiver PID in `/proc`, discovers its port, and probes it locally. There is no reserve-then-release port race.
4. Before each trial, unlink only the previous owned `payload` and recheck free space. rsync therefore always transfers the complete file, not an unchanged-file shortcut. Its temporary output and gosync's output occupy the same filesystem, and there is only one ISO-sized output at a time.
5. Run the transfer command with no progress UI:

   ```text
   gosync -transport server -workers 1 -checksum -progress=false SOURCE HOST:PORT:/payload
   rsync -rt --whole-file --ignore-times --no-compress --stats \
     --protect-args --rsync-path=<bounded remote Python rsync wrapper> \
     -e 'ssh -o BatchMode=yes -o Compression=no ...' SOURCE HOST:<owned-data-directory>/payload
   ```

   Additional SSH options bound connection setup and dead-peer detection. SSH's resolved `hostname` from `ssh -G` is used for the direct TCP connection; SSH still uses the original alias for management and rsync.
6. Outside the timer, stream remote SHA-256 over the output after **every** trial, including warmups. Check both digest and length against the source. Verification is attempted even after a failed command; failure aborts further trials. Source size, modification time, and inode are also checked between trials.
7. Warmup rounds and measured rounds each alternate **AB, BA, AB, ...**, with A = gosync and B = rsync. The default warmup is one trial per tool. Warmups are retained in JSON but excluded from medians.
8. In `finally`, attempt cleanup whenever preparation was attempted, even if its SSH response was lost. Stop only tracked owned processes and remove only the two owned directories. Both directories must pass ownership checks before any process is signaled, and ownership is checked again before removal. Missing/mismatched markers and directory/marker symlinks cause refusal; already-absent directories are harmless. PID/start-time checks guard signaling; no broad `pkill`, user-file deletion, or `-deploy` is used.

## Timing and results

Each invocation writes a unique `benchmark-transfer-<id>.json` into `--output`, checkpoints completed trials, and prints its path plus summary. Existing result files are not overwritten. Exit status is nonzero on transfer, verification, or cleanup failure.

- Wall time uses `time.perf_counter()` around the local child command, including launch, connection establishment, transfer, built-in protocol checksums, and exit. Setup, external source/output hashing, CPU probes, capacity checks, and cleanup are excluded for both tools.
- Local child user/system CPU deltas use `resource.getrusage(RUSAGE_CHILDREN)`, including local SSH descendants for rsync. The runner runs commands sequentially so these deltas are not contaminated by its other command children.
- Optional remote receiver user/system CPU snapshots come from `/proc/<pid>/stat` immediately around gosync commands, in clock-tick units converted to seconds. Snapshot failures are recorded without failing the trial. These are not remote rsync CPU measurements and not whole-system CPU measurements.
- Every trial includes argv, return code, timeout flag, raw decoded stdout/stderr, wall/CPU times, and verification results. Non-UTF-8 bytes are replaced when decoding. Setup/cleanup command records and receiver logs are also captured.
- Summaries contain measured-trial median wall seconds, median **decimal MB/s** (`bytes / seconds / 1,000,000`), and median **MiB/s** (`bytes / seconds / 1,048,576`). They describe source-file throughput, not measured network bytes. Partial/failed runs do not publish a complete summary.
- Metadata includes local/remote tool versions, Python/platform/CPU counts, remote `/proc/cpuinfo`, `/proc/meminfo`, mount information, filesystem statistics, and available capacity. Reports contain hostnames, paths, and environment information; review before sharing.

## Bounds and limitations

Local subprocesses run in their own process groups, with timeouts, group termination, and reaping. SSH helpers have their own remote alarm deadlines. A remote Python wrapper bounds each rsync process group independently of the SSH client's lifetime. Receiver and rsync PIDs are recorded with Linux process start times; cleanup refuses to signal a reused PID. SIGINT/SIGTERM trigger cleanup, and further interrupts are ignored during that bounded attempt.

A detached supervisor also caps the receiver's total lifetime to
`2 * (runs + warmups) * (6 * timeout + 30) + 120` seconds. If that lifetime expires, it kills/reaps its receiver and logs that artifacts are retained. **The supervisor never removes directories**, on expiry or early receiver exit: it cannot safely assume that rsync has stopped or that directory ownership is unchanged. The rsync wrapper retains its independent per-command deadline. Normally the local runner stops the tracked rsync process, supervisor, and receiver, then collects logs and removes the ownership-checked directories itself.

Cleanup is **best effort**, not transactional across SSH failures. The caller knows and checkpoints both paths and the token before preparation, so a lost preparation response no longer prevents a cleanup attempt. A machine crash, SIGKILL, uninterruptible kernel I/O, loss of connectivity, or a still-running helper can nevertheless prevent cleanup. Interruption between directory creation and marker completion leaves an unmarked directory that automatic cleanup deliberately refuses. A collision or ownership refusal can also leave partially prepared owned artifacts. If caller cleanup fails after stopping the supervisor, there is no later directory-removal fallback; even successful lifetime expiry only bounds the receiver, not artifact retention. Inspect the report's exact paths, ownership markers, and recorded PID/start-time identities, and ensure all writers have stopped before manually removing artifacts. Do not remove a path with missing/mismatched ownership evidence without independently establishing ownership. Never use broad process-name kills. No user files are used as receiver outputs.

This is a **warm-cache, end-to-end transport comparison**, not an equal-crypto microbenchmark: rsync uses encrypted SSH, whereas gosync's server path is plain TCP and `-checksum` adds gosync's own integrity work inside the timer. External SHA-256 verification is identical for both. Timings include each tool's normal startup costs (including the small remote Python wrapper for rsync). Warmups and alternating order reduce, but do not eliminate, cache, CPU-frequency, thermal, network, and memory-pressure bias. No fsync/durable-storage throughput is being measured on tmpfs.

The receiver binds an ephemeral TCP port on all IPv4 interfaces (possibly dual-stack according to Go/Linux behavior). The benchmark does not add authentication, encryption, a firewall rule, or a security sandbox to gosync serve. **Use a trusted, access-restricted benchmark network**; other clients must not connect. The chosen port must be reachable directly from the local machine. SSH ProxyJump/ProxyCommand connectivity alone does not guarantee direct TCP connectivity. Simple SSH aliases/DNS names/IPv4 and optional `user@host` are supported; IPv6 destination syntax is not. Remote HOME staging paths must contain only letters, digits, underscore, dot, slash, and hyphen for portable scp handling. Requested destination paths may contain spaces and shell metacharacters: helper arguments are JSON plus `shlex`-quoted command arguments, and rsync uses protected arguments.

The runner does not change existing services, build the binary, validate the advertised tmpfs mount against a policy, or run the benchmark as part of installation. Review the emitted versions to ensure the intended binary was used.

## Local regression tests

```sh
python3 -B -m unittest discover -s scripts -p 'test_benchmark_transfer.py' -v
```

These standalone stdlib tests use local temporary directories and mocked helper dependencies. They exercise buffer-size parsing and range checks, both gosync endpoint commands (including default flag omission), unchanged rsync commands, JSON argument recording, subprocess deadlines, exclusive preparation, ownership refusal, PID reuse checks, safe expiry retention, and caller cleanup after a lost preparation response. They never use SSH or change remote state.
