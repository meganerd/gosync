# Archive-stream directory transfers

This release adds experimental `--directory-mode=archive` support for TCP,
server, QUIC, and deployed receivers. Existing transfers continue to use the
per-file path unless the option is supplied.

- Streams directory data without constructing or staging a complete archive.
- Preserves nested and empty directories, zero-length files, safe permissions,
  modification times, and policy-supported symbolic links.
- Negotiates version and policy independently of legacy `CAPS`; old peers fail
  before archive entries are sent.
- Enforces bounded frames, extraction-root confinement, safe link targets,
  unsupported-object rejection, CRC32C, per-file SHA-256, archive SHA-256, and
  cleanup of incomplete staged files.
- Adds `--archive-policy=strict|portable|best-effort`; portable remains the
  archive-mode default.

Archive protocol version 1 intentionally has no mid-stream resume. Operators
should retry the complete archive after interruption. TCP/server traffic remains
unencrypted; QUIC retains its existing TLS verification requirements.
