# MEG-38: gosync current state compared with the MEG-22 Harbor proposal

## Decision

The MEG-22 proposal is not a proposed state for `gosync`. It is an
infrastructure-service design for the separate **Infrastructure Work** project.
No Harbor Helm values, Kubernetes manifests, credentials, or operating runbooks
should be added to this repository as part of MEG-38.

The systems have one consumer/provider relationship: gosync's GitLab pipeline
currently pulls `golang:${GO_VERSION}` from a container registry. A Harbor
pull-through cache could serve that dependency after the infrastructure project
deploys it and the GitLab runner or CI image reference is deliberately migrated.
That later integration is optional and must preserve a tested direct-upstream
rollback.

## Sources and comparison boundary

This comparison uses:

- the MEG-38 issue description, which reproduces the MEG-22 proposal;
- the approved MEG-20 plan and its linked project metadata;
- the completed MEG-21 discovery record and status;
- the current MEG-22 issue metadata and comments; and
- repository state on `main` at commit `206b15e`.

MEG-20, MEG-21, and MEG-22 are records in **Infrastructure Work**. MEG-38 is
attached to **gosync**. MEG-22 remains in progress and has its own platform
reliability owner. This document does not attempt to replace or pre-empt that
owner's implementation-ready Harbor design.

## Current gosync state

| Area | Current state | Relationship to MEG-22 |
| --- | --- | --- |
| Product | Native Go file-transfer CLI and receiver | Harbor is an OCI registry/cache, not a replacement or target architecture for gosync |
| Distribution | Cross-compiled Linux, macOS, and Windows binaries | No first-party OCI image or Kubernetes workload is defined |
| Runtime | QUIC, TCP, and SSH/deployment transfer paths | Does not require Kubernetes, Helm, a registry, PostgreSQL, or Redis |
| Remote deployment | Copies and starts a native binary over SSH, then transfers over QUIC or TCP | Does not pull an image from Harbor |
| CI | GitLab CI uses `golang:${GO_VERSION}` for tests and builds | This public image is a possible future Harbor-cache consumer |
| Deployment configuration | CLI flags, environment variables, and optional JSON configuration | No cluster, namespace, ingress, TLS issuer, PVC, or registry-client configuration |
| Security boundary | SSH host-key verification and QUIC TLS trust/leaf pinning; TCP is unencrypted | Independent of Harbor authentication, proxy-project RBAC, and upstream registry credentials |
| Persistence | User-selected source and destination files | No registry blobs, database, Redis state, retention policy, or garbage collection |
| Operations | Application tests, benchmarks, and transport documentation | No Harbor backup/restore, capacity, availability, or upgrade procedures |

The repository contains no first-party Dockerfile, Compose definition, Helm
chart, Kubernetes manifest, Harbor configuration, or registry credential. YAML
owned by copied benchmark source trees is third-party experiment material and is
not a gosync deployment surface.

## MEG-22 acceptance-criteria mapping

| MEG-22 requirement | State in gosync | Finding |
| --- | --- | --- |
| Map discovery inputs to Harbor, k3s, storage, ingress, TLS, and network configuration | No corresponding configuration | Out of scope here; MEG-21 discovery and MEG-22 design belong to Infrastructure Work |
| Pin supported Harbor chart/application versions and provide reproducible configuration | No Harbor dependencies or artifacts | Missing by design, not a gosync defect |
| Define proxy-project, authentication, and upstream-credential boundaries | No registry proxy projects or credentials | Infrastructure responsibility; secrets must not enter this repository |
| Design backup, restore, retention, garbage collection, upgrade, and capacity procedures | No Harbor state or lifecycle | Infrastructure responsibility |
| Justify any departure from k3s using MEG-20 revision 2 fallback conditions | gosync is not a Harbor hosting candidate | The fallback test cannot be evaluated from this repository and must remain with MEG-22 |

The MEG-22 acceptance criteria are therefore neither implemented nor required
for gosync. Treating them as gosync gaps would conflate application source with
shared platform infrastructure and create conflicting ownership.

## Safe integration points after Harbor exists

No integration change should be made until MEG-22's design and the dependent
Harbor pilot are complete. Afterwards, a small, separately reviewed gosync task
may evaluate CI use of the cache:

1. Confirm which GitLab runner and runtime pull `golang:${GO_VERSION}` and the
   exact proxy-project path/rewrite established by the Harbor design.
2. Prefer a digest-pinned builder image where the release workflow permits it.
3. Change only the CI image reference or runner mirror configuration selected by
   the infrastructure owner; do not add Harbor credentials to the repository.
4. Run the existing test and cross-build jobs through the cache and record a
   cold pull, warm pull, and authentication-failure result.
5. Test rollback to the direct upstream before rollout. A cache outage must not
   silently change trust or credential policy.

Creating and publishing a gosync OCI image would be a separate product decision.
The current native-binary release model supports the documented platforms and
should remain unchanged unless such a change is explicitly approved.

## Disposition

- **Repository change required now:** this comparison document only.
- **gosync code/configuration change:** none.
- **MEG-22 work:** continue under its existing Infrastructure Work owner.
- **Potential follow-up:** after the Harbor pilot is operational, evaluate the
  GitLab builder-image cache path and rollback as a separate gosync integration
  task if the infrastructure owner confirms the endpoint and access model.

