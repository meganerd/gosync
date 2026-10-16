# Archive-stream compatibility and operations

Archive streaming is an opt-in protocol-v1 feature. It transfers a directory as
one bounded, checksummed stream without constructing a tar file. Both endpoints
must be this gosync version or newer. There is no downgrade to the legacy
per-file protocol and no mid-stream resume.

## Implemented compatibility matrix

The table describes the current v1 implementation, not future fields reserved
by the protocol design. `Preserve` means represented and verified end to end;
`skip + warning` is a successful transfer whose warning is returned to the
operator; `reject` is a failed transfer.

| Object or metadata | strict | portable (default) | best-effort |
|---|---|---|---|
| regular file contents and tree | Preserve | Preserve | Preserve |
| empty directories and zero-length files | Preserve | Preserve | Preserve |
| modification time | Preserve to destination filesystem precision | Same | Same |
| permission bits | Preserve where the receiver supports `chmod`; Windows applies its local approximation | Same | Same |
| safe relative symlink | Preserve, or reject if receiver policy/privilege prevents creation | Skip + `skipped symlink: PATH` | Preserve, or reject if creation fails |
| sparse allocation | Not represented: logical bytes are transferred and allocation may expand | Same | Same |
| owner, group, ACL, xattr, atime, Windows DACL/ADS | Not represented in v1 | Not represented in v1 | Not represented in v1 |
| FIFO, socket, device, or other special entry | Reject: `unsafe or unsupported archive entry "PATH"` | Reject | Reject |
| unsafe path/link, case collision on Windows/macOS, existing symlink parent | Reject | Reject | Reject |

Because owner/ACL/xattr and sparse-layout records are not emitted by the current
wire implementation, `strict` means exact preservation of the v1 represented
subset. Do not use archive v1 when those attributes are required. A sparse file
can consume its full logical size at the destination.

Path components on the wire always use `/`. Absolute paths, `..`, repeated
separators, backslashes, drive (`C:`), UNC/device prefixes, NUL/control bytes,
and over-limit components are rejected. Windows additionally rejects trailing
dots/spaces and `CON`, `PRN`, `AUX`, `NUL`, `COM1`-`COM9`, and `LPT1`-`LPT9`
(case-insensitive, including extensions). Unicode names are carried as UTF-8
bytes. Case collisions are rejected on Windows and macOS; operators must also
preflight canonically equivalent Unicode names because v1 does not normalize
them and filesystem normalization differs.

## Sender and receiver expectations

| Sender -> receiver | Expected v1 result |
|---|---|
| Linux -> Linux | Content/tree/mtime/mode; symlinks according to policy |
| Linux -> macOS | Content/tree/mtime/mode; case collisions reject; symlinks according to policy |
| Linux/macOS -> Windows | Content/tree/mtime; mode is a Windows approximation; reserved names and case collisions reject; symlinks require Windows privilege outside portable mode |
| Windows -> Linux/macOS | Content/tree/mtime; Windows permission/DACL identity is not represented; receiver applies transmitted permission bits |
| Same platform | Same rules; destination filesystem capability can still reduce timestamp precision or reject links |

Warnings are deterministic and their exact text is printed as `Archive warning:
MESSAGE` only after the receiver's final archive acknowledgement (`--quiet`
suppresses them with the rest of the summary). Any rejection requires retrying
the complete archive.
Completed regular files are staged, synced, checksum-verified, and renamed into
place. An existing regular file at the same path is replaced only after the new
file verifies. Existing symlinks are never followed as destination parents.

## Safe operational examples

For an isolated trusted network, TCP can replace `tar | netcat` while adding
bounds, path confinement, checksums, acknowledgement, and atomic regular-file
replacement:

```sh
# Receiver: firewall port 9444 to the sender address; base confines destinations.
gosync serve --transport tcp --listen 10.0.0.20:9444 --base /srv/import

# Sender: portable is the predictable mixed-platform default.
gosync --transport tcp --directory-mode=archive --archive-policy=portable \
  /srv/export sender-approved-host:9444:/daily/export
```

TCP is plaintext and has no client authentication. Across untrusted or shared
networks, use QUIC and verify a receiver certificate obtained over a trusted
channel:

```sh
gosync serve --transport quic --listen :9444 --base /srv/import \
  --cert-out /secure/receiver-leaf.pem
gosync --transport quic --cert ./receiver-leaf.pem \
  --directory-mode=archive --archive-policy=portable \
  ./source receiver.example:9444:/incoming/source
```

Use `strict` only when failure is preferable to a skipped link or receiver
translation. Use `best-effort` when safe symlink creation should be attempted.
Neither mode opts into special files. Limit receiver network access, verify the
QUIC certificate or SSH host key used by `--deploy`, and never expose an
unauthenticated TCP receiver to the public Internet.

## Verification routing

Platform-neutral protocol and policy tests run in the normal `go test ./...`
job. Native filesystem behavior is isolated in `archive_linux_test.go` and
`archive_windows_test.go` with Go build tags; macOS/Windows release validation
must execute `go test ./pkg/archive` on native runners, not merely cross-compile.

Focused verification on 2026-10-16 (Linux/amd64, Ryzen 9 7950X) passed
`go test -count=1 ./pkg/archive ./pkg/server ./pkg/transport ./pkg/sync`; the
archive test binary also cross-compiled for Windows/amd64 and Darwin/amd64.
Three 8 MiB send-only benchmark samples measured 1.13-1.16 GB/s with about
80.6 KiB and 160 allocations per operation. The deliberately minimal
`io.CopyBuffer` baseline measured 9.72-10.23 GB/s and 3 allocations; it excludes
archive framing, metadata, CRC32C, and SHA-256 and is a lower-bound comparison,
not an equivalent transfer.
