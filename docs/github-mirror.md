# GitHub mirror operations

GitLab remains the authoritative repository for gosync. The GitLab pipeline
publishes its default branch and tags to GitHub, and imports open GitHub pull
requests into review-only GitLab branches.

## Required CI/CD variables

Configure this protected, file-type variable in the GitLab project:

- `GITHUB_MIRROR_SSH_KEY`: the private half of a dedicated GitHub deploy key
  with read/write access to `github.com/meganerd/gosync`.

GitLab inbound writes use the pipeline's short-lived `CI_JOB_TOKEN`. Enable
"Allow Git push requests to the repository" for CI job tokens in the GitLab
project settings. Do not put credentials directly in `.gitlab-ci.yml` or in a
repository remote.

## Pipeline behavior

- `mirror:github:push` runs after successful default-branch and tag pipelines.
  It updates the GitHub default branch and publishes all GitLab tags.
- `mirror:github:pull-requests` runs in scheduled pipelines or can be started
  manually from a web pipeline. Each open GitHub PR is written to
  `github-pr/<number>` in GitLab.

Create a GitLab merge request from the imported `github-pr/<number>` branch to
the default branch. The inbound job never updates the default branch, so an
external GitHub contribution still passes the normal GitLab review and CI path.

Create a scheduled pipeline (for example, every 15 minutes) on the default
branch to keep GitHub PR branches current. Closed PR branches are intentionally
retained; delete them after the corresponding GitLab merge request is merged or
closed.

The mirror jobs use an empty `needs` dependency list so repository
synchronization can proceed even if an unrelated test or build job fails.

## Local verification

The synchronization script can be tested against temporary bare repositories:

```sh
GITHUB_MIRROR_URL=/path/to/github.git \
CI_DEFAULT_BRANCH=main \
./scripts/sync-github-mirror.sh push

GITHUB_MIRROR_URL=/path/to/github.git \
GITLAB_MIRROR_PUSH_URL=/path/to/gitlab.git \
./scripts/sync-github-mirror.sh pull-requests
```
