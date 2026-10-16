# Archive-stream directory transfer protocol

Status: **proposed**, version 1, not implemented. Tracking issue: MEG-99.

This document is the review boundary for an in-process, streaming directory transfer
equivalent to `tar | netcat` and `netcat | tar`. It specifies protocol and policy only;
implementation must not begin until this proposal is accepted.

## 1. Goals, non-goals, and compatibility

Archive-stream mode sends one directory tree over one ordered byte stream without requiring
`tar`, `netcat`, or another external program. It must:

- use bounded memory and natural transport backpressure;
- recreate only objects allowed by the selected metadata policy;
- detect corruption and incomplete archives before reporting success;
- never resolve an archive path outside the receiver's destination root; and
- leave current single-file and per-file directory transfers unchanged.

Version 1 does not provide compression, encryption, deduplication, random access, or
mid-archive resume. Encryption and peer authentication remain transport concerns (QUIC has
TLS; TCP/server does not). Existing `SEND`, `SEND-NOHASH`, `RECEIVE`, `RECEIVE-NOHASH`,
fan-out, SSH, and the current scanner/worker directory path keep their present framing and
semantics. Archive mode is explicit (proposed CLI: `--directory-mode=archive`); the default
remains `--directory-mode=files`. A single-file source never uses this protocol.

An archive request against an old receiver fails before sending an entry. It does not
silently fall back, because fallback could weaken a requested metadata policy. A caller may
explicitly choose the existing `files` mode and retry.

## 2. Negotiation

The existing `CAPS` reply is compared byte-for-byte and remains exactly
`OK CAPS NOHASH`. Archive discovery therefore uses a separate request on a fresh control
stream/connection:

```text
C: ARCHIVE-CAPS 1
S: OK ARCHIVE-CAPS 1 <base64url-capabilities>
```

`1` is the sender's highest supported major version. The response selects one major
version. An unknown command, malformed response, unsupported version, or timeout means
archive mode is unavailable and no archive bytes may follow. Capabilities are canonical
UTF-8 JSON encoded with unpadded base64url. Unknown JSON keys are ignored. Version 1 keys:

```json
{
  "os": "linux|darwin|windows",
  "path_case": "sensitive|insensitive|preserving",
  "max_frame": 1048576,
  "max_path": 4096,
  "features": ["mode", "uid_gid", "mtime_ns", "atime_ns", "symlink",
               "hardlink", "sparse", "xattr", "acl_posix", "acl_nfs4",
               "acl_windows"],
  "hashes": ["sha256"],
  "policies": ["strict", "portable", "best-effort"]
}
```

Feature names describe operations the receiver can safely perform in its current build and
runtime environment, not merely OS theory. Privilege-dependent features such as ownership
or devices must be omitted unless usable for this transfer. `device` and `fifo` are valid
capabilities but are disabled by default and require the explicit unsafe-object gate in
section 6. A sender intersects capabilities, chooses one policy and hash, then starts:

```text
C: ARCHIVE-BEGIN 1 <strict|portable|best-effort> sha256 <base64url-options>
S: OK ARCHIVE-BEGIN <128-bit-hex-transfer-id> <max-frame>
```

Options are canonical JSON and contain `unsafe_special_files` (default `false`) and
receiver limits requested by the sender. The receiver may only lower limits. It rejects an
unsupported policy, hash, option, or required feature before returning a transfer id.

Capability advertisement is not a promise that every entry will succeed: filesystem mount
options, permissions, quotas, and races can still fail. Such failures use the policy and
error rules below.

## 3. Record framing and archive grammar

After `ARCHIVE-BEGIN`, the same ordered full-duplex stream carries binary records. Integers
are unsigned big-endian. The fixed 32-byte header is:

| Offset | Size | Field | Rule |
|---:|---:|---|---|
| 0 | 4 | magic | ASCII `GSA1` |
| 4 | 1 | record type | listed below |
| 5 | 1 | flags | unknown set bits are fatal |
| 6 | 2 | header length | exactly 32 in v1 |
| 8 | 8 | sequence | starts at 0 and increases by 1 |
| 16 | 8 | payload length | at most negotiated `max_frame` |
| 24 | 4 | payload CRC32C | Castagnoli; zero only for empty payload |
| 28 | 4 | reserved | must be zero |

CRC32C protects framing and gives early failure; SHA-256 supplies end-to-end integrity.
Metadata payloads are canonical CBOR: definite lengths only, shortest integer encoding,
UTF-8 map keys in bytewise lexical order, no floats, duplicate keys, tags, or indefinite
items. Unknown record types and unknown required metadata keys are fatal. Keys prefixed
`optional.` may be skipped subject to policy, and generate a warning.

Record types are `ROOT(1)`, `ENTRY(2)`, `DATA(3)`, `HOLE(4)`, `END_ENTRY(5)`,
`END_ARCHIVE(6)`, `CANCEL(7)`, `WARNING(8)`, and `ERROR(9)`. Sender records use even
sequence space starting at 0; receiver control records use odd sequence space starting at
1, so either direction can be decoded independently. Records are not interleaved at the
byte level.

The sender grammar is:

```text
ROOT
(ENTRY (DATA|HOLE)* END_ENTRY)*
END_ARCHIVE
```

`ROOT` contains a random 128-bit archive id, sender OS, selected policy, and declared
limits. `ENTRY` contains:

- monotonically increasing `entry_id` (uint64);
- normalized relative `path` (UTF-8 with `/` separators);
- `kind`: `directory`, `regular`, `symlink`, `hardlink`, `fifo`, `char_device`, or
  `block_device`;
- regular-file logical `size`; and
- applicable metadata: mode, uid/gid plus optional names, timestamps in signed Unix
  nanoseconds, link target or prior hardlink entry id, sparse intent, xattrs, and typed ACL.

`DATA` contains entry id, logical offset, byte count, then bytes. Frames for a regular file
must be contiguous except for `HOLE` extents, must not overlap, and must cover exactly its
declared logical size. `HOLE` contains entry id, offset, and length and advances the logical
cursor without transmitting zeros. Receivers lacking safe sparse support write zeros in
portable/best-effort mode and reject in strict mode.

`END_ENTRY` carries entry id, logical size, and SHA-256 over the logical file contents
(holes hash as zero bytes). It is absent for non-regular entries. The receiver verifies the
size and digest before closing/staging the file. `END_ARCHIVE` carries entry count, total
logical bytes, and SHA-256 over every preceding complete wire record (header plus payload).
The receiver acknowledges only after validating it and completing final metadata:

```text
receiver: END_ARCHIVE {archive_id, status="ok", warnings, archive_sha256}
```

Success means every accepted object and required metadata operation completed. It does not
imply durable media flush in v1; a future negotiated durability feature may add that.

Directories are emitted before descendants. Hard links may reference only an earlier
regular-file entry. Directory permissions and timestamps are applied in reverse depth order
after descendants, preventing child creation from destroying final directory timestamps.

## 4. Flow control, cancellation, and errors

The sender writes at most one negotiated frame at a time through the transport. It does not
queue an unbounded archive or whole file. A blocking write is backpressure. QUIC stream
flow control or the TCP socket window is authoritative; v1 adds no second credit window.
Implementations use a bounded buffer pool and stop reading the source while a network write
is blocked.

Either peer may send `CANCEL` with code and UTF-8 reason, then half-close its sending side.
The other peer stops filesystem work, removes staging objects it created where possible,
answers `CANCEL`, and closes. Context cancellation, operator interrupt, or timeout uses this
path. If the stream is already unusable, transport close is an implicit cancellation.

`WARNING` contains entry id (or zero), stable machine code, path, and explanation. Warnings
are counted and returned in the final acknowledgement and CLI summary. `ERROR` contains the
same fields plus `retryable`. After `ERROR`, no success acknowledgement is legal; the peer
answers `CANCEL` and closes. Stable initial codes include `PROTOCOL`, `LIMIT`, `PATH_UNSAFE`,
`NAME_COLLISION`, `UNSUPPORTED_METADATA`, `UNSAFE_OBJECT`, `IO`, `CHECKSUM`, `SOURCE_CHANGED`,
and `CANCELLED`. Human strings are diagnostic and must not be parsed.

A source regular file changing size or modification identity while read is
`SOURCE_CHANGED`; it is never reported as a valid snapshot. Archive mode is not an atomic
filesystem snapshot and documents this limitation.

## 5. Resume and commit expectations

Version 1 deliberately has **no mid-stream resume**. Archive ordering, filesystem mutation,
and link dependencies make a trustworthy resume journal substantially more complex than
the current per-file resume. A failed archive receives a new archive id and restarts from
`ROOT`. `ARCHIVE-RESUME` is reserved and must be rejected in v1.

Files are written to receiver-created, non-followed staging names in their final parent and
renamed into place after their size and digest pass. An interrupted archive may therefore
contain already committed entries, but it must never expose a partial regular file at its
final path. Restart semantics are overwrite/update semantics for the paths in the new
archive; archive mode does not delete unrelated destination entries and is not globally
atomic. A receiver removes known staging files on failure and may garbage-collect stale
staging files by age and ownership marker.

The final acknowledgement may be lost after a successful extraction. Retrying is safe for
regular files and directories under overwrite policy, but not guaranteed idempotent for
all metadata or special files. Automation must treat this as indeterminate and verify or
retry the whole archive; it must not assume rollback.

## 6. Path and link safety model

All checks occur before creating an entry, using the original UTF-8 path and a component
walk rooted at a receiver-opened destination directory handle.

1. Paths must be non-empty relative paths with `/` separators. Reject a leading `/`, `\`,
   repeated empty component, `.`, `..`, NUL, invalid UTF-8, C0 control character, Windows
   drive/UNC/device prefix, colon in a Windows component, trailing dot/space on Windows,
   and Windows reserved device names (case-insensitive, with or without extension).
2. Enforce negotiated encoded-byte length, component count, component length, entry count,
   total logical bytes, per-file size, xattr/ACL bytes, and link count before allocation.
   Proposed defaults are 4 KiB path, 255-byte component, 1,000,000 entries, 16 TiB logical
   archive, 8 TiB file, 1 MiB metadata per entry, and 64 KiB per xattr. Operators may lower
   them; increases require explicit configuration.
3. Normalize separators only; do not Unicode-normalize names. Build a receiver comparison
   key using its actual case/normalization behavior and reject two archive paths that map to
   one destination name. Case or Unicode collisions are never last-writer-wins.
4. Walk parents without following links or reparse points. On Unix use descriptor-relative
   operations (`openat2` with `RESOLVE_BENEATH|RESOLVE_NO_SYMLINKS|RESOLVE_NO_MAGICLINKS`
   where available, otherwise an `openat`/`fstatat` no-follow component walk). On Windows
   open relative to the root handle and reject reparse points in every existing component;
   verify the resolved handle remains under the root. String-prefix checks alone are not
   sufficient.
5. Existing destination symlinks/reparse points at any component, including the leaf, are
   rejected. Replacement uses no-follow handles and same-directory atomic rename where the
   platform permits. A type change requires explicit overwrite policy and must never
   recursively delete an unlisted directory.
6. A symlink target is stored as text only after validation. Reject absolute, drive, UNC,
   device, NUL, or targets whose lexical resolution from the link's parent escapes root.
   The receiver does not dereference the target during extraction. Strict mode additionally
   rejects a target that cannot be proven to name an in-archive path. Portable mode skips
   symlinks with warning. Best-effort creates only validated symlinks when supported.
7. Hard links refer by prior entry id, never by free-form target path. The target must be a
   committed regular file inside this archive and inside the same destination root.
8. Device nodes and FIFOs require both negotiated support and
   `unsafe_special_files=true`. They are rejected by default in **all** policies; best-effort
   must never silently create them. Socket entries are not representable in v1.

These rules must be enforced at use time as well as parse time to resist directory-swap
TOCTOU attacks by another local process.

## 7. Metadata policies

The selected policy applies to both advertised limitations and runtime failures:

- **strict**: preserve every represented object, name, and metadata field exactly. Reject
  before `ROOT` when negotiation proves that impossible; otherwise abort on the first skip,
  translation, precision loss, privilege failure, name collision, or unsupported feature.
- **portable** (default): preserve regular files, directories, contents, mtime to receiver
  precision, and owner read/write intent. Translate mode to a safe receiver equivalent.
  Skip symlinks, hard-link identity, sparse allocation, ownership, xattrs, and ACLs when not
  portable, always with a structured warning. Reject name collisions and all unsafe special
  objects. A skipped symlink is not materialized as a text file.
- **best-effort**: attempt every safe negotiated feature, translate when exact preservation
  is impossible, and warn for each translation or skip. Content, directory structure,
  checksums, path safety, and name uniqueness remain mandatory. Runtime failures affecting
  file content or structure are fatal. Special files still require the separate unsafe gate.

No policy suppresses warnings. A CLI exit is success with warnings only after the receiver's
final acknowledgement; callers may request `--warnings-as-errors` locally.

## 8. Platform capability matrix

`P` = normally preserved, `T` = translated or precision-limited with warning, `S` = safely
skipped with warning outside strict mode, `R` = rejected unless an explicit gate is noted.
Actual handshake capability and destination filesystem may reduce these baselines.

| Feature | Linux receiver | macOS receiver | Windows receiver | Portable default |
|---|---|---|---|---|
| POSIX rwx/mode | P | P | T to read-only/ACL semantics | T to safe local permissions |
| uid/gid ownership | P with privilege | P with privilege | S (no direct identity map) | S |
| mtime | P, ns if FS supports | P, precision FS-dependent | P, 100 ns representation | P/T precision |
| atime | P, mount may alter | P | P | S |
| symlink | P | P | P only with OS privilege/policy | S |
| hard link | P, same filesystem | P, same filesystem | P on NTFS/ReFS same volume | S identity; duplicate content |
| sparse extents | P where FS supports | P where FS supports | P on sparse-enabled NTFS/ReFS | T to zero-filled file |
| xattrs | P, namespace/limits vary | P, names differ | S (ADS is not a safe generic map) | S |
| POSIX ACL | P where enabled | T to macOS/NFSv4 model | S | S |
| NFSv4/macOS ACL | T/FS-dependent | P | T only with explicit mapper | S |
| Windows DACL | S | S | P | S |
| char/block device | gated; privilege required | gated; privilege required | R | R |
| FIFO | gated | gated | R | R |
| case-distinct names | P on typical FS | FS-dependent, usually collision | collision on typical FS | R on collision |

Cross-platform decisions follow the receiver column, independent of sender: sender metadata
is tagged by model, the receiver either preserves that model, applies a documented
translation, skips with warning, or rejects under strict. In particular:

| Sender -> receiver | strict | portable (default) | best-effort |
|---|---|---|---|
| Linux -> macOS | reject Linux-only xattrs/ACLs, unsupported ownership/precision | content/tree/mtime; safe mode translation; skip identity metadata | attempt POSIX metadata; warn on namespace/ACL translation |
| Linux/macOS -> Windows | reject POSIX mode/owner/xattr/ACL mismatch and unprivileged links | content/tree/mtime; read-only translation; links skipped/links duplicated as content only for hard links | attempt links and sparse files; warn for every permission/metadata translation |
| Windows -> Linux/macOS | reject DACL or name/time semantics that cannot be exact | content/tree/mtime; translate read-only; skip DACL | apply safe mode approximation; warn that DACL identity is lost |
| Same OS/model | reject any runtime inability to preserve | portable subset only | preserve all safe advertised features, warning on filesystem limits |

Hard-link duplication in portable mode means the sender emits the referenced regular
content as another regular entry; it is not a receiver-side skip and remains checksummed.
The sender must decide this before emitting `ENTRY`. Case/normalization collision is always
`NAME_COLLISION`, regardless of policy.

## 9. Testable conformance requirements

An implementation is conformant only if automated tests cover:

1. byte-level golden vectors for every record, canonical CBOR rejection, CRC corruption,
   unknown flags/types, sequence gaps, truncation, overflow, and SHA-256 mismatch;
2. negotiation with old peers, exact preservation of legacy `CAPS`, unsupported versions,
   feature intersections, and no archive bytes before successful `ARCHIVE-BEGIN`;
3. bounded-memory transfer through a deliberately slow reader and cancellation while both
   sender and receiver are blocked;
4. rejection of absolute, traversal, mixed-separator, drive, UNC, device-name, NUL,
   over-limit, symlink-parent, reparse-parent, escaping-link, hardlink-forward-reference,
   case-collision, and Unicode-normalization-collision fixtures;
5. adversarial replacement of a checked parent directory with a symlink/reparse point
   between validation and creation, proving descriptor/handle containment;
6. short data, overlaps, gaps, sparse logical digest, source mutation, cleanup after error,
   loss of final acknowledgement, and absence of partial final files;
7. a generated policy matrix test for every feature × policy × receiver capability outcome:
   `preserve`, `translate+warning`, `skip+warning`, or `reject`;
8. Linux, macOS, and Windows integration jobs on native filesystems, including reduced
   privilege runs. Privilege-gated special-file tests must prove the default is rejection;
9. unchanged single-file protocol golden tests and unchanged per-file directory integration
   tests.

Cross-OS CI may use archived fixtures produced on each source OS, but extraction assertions
must run on the native receiver OS. Filesystem-specific exceptions are capability-driven
and visible as warnings, never hard-coded silent passes.

## 10. Upgrade strategy and observability

Version 1 ships behind an explicit experimental archive-mode flag. Recommended rollout:

1. land parser, validation, and golden/adversarial tests without exposing a CLI path;
2. add receiver support and capability advertisement, still default-off;
3. add sender support for QUIC and TCP/server with portable policy as default;
4. exercise mixed-version and native-OS CI; then document the experimental CLI;
5. only consider changing the directory default in a separately approved breaking-change
   proposal with field evidence and a deprecation period.

Major versions use a new highest-version negotiation and may change framing. Minor additions
use optional capability/metadata keys; v1 peers ignore only `optional.*` keys. Senders must
never emit a feature the receiver did not advertise. Receivers must keep archive discovery
separate from `CAPS` for legacy compatibility.

Operational metrics should count archives started/completed/cancelled, bytes logical/wire,
entries by kind, warning/error codes, staging cleanup, checksum failures, and duration spent
blocked on transport writes. Paths, link targets, owner names, and xattr/ACL values are not
telemetry fields. Logs may include a transfer id and entry id; paths require an explicit
verbose/debug setting and local redaction policy.

## 11. Deferred decisions

The following require later, separately reviewable proposals: resumable journals and their
authentication, compression, content-defined deduplication, global atomic tree swaps,
deletion/mirroring semantics, socket objects, Windows ADS mapping, a general cross-model ACL
translator, and durable (`fsync`/volume flush) completion. None may be inferred from v1.
