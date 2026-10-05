# GitHub mirror operations

GitLab remains the authoritative repository for gosync. The GitLab pipeline
publishes its default branch and tags to GitHub, and imports open GitHub pull
requests into review-only GitLab branches.

## Required CI/CD variables

Configure these masked, protected variables in the GitLab project:

- `GITHUB_MIRROR_URL`: an authenticated Git URL with read/write access to
  `github.com/meganerd/gosync`, such as an SSH URL using a dedicated deploy key.
- `GITLAB_MIRROR_PUSH_URL`: an authenticated Git URL with permission to create
  branches in the GitLab project. It is used only by the inbound job.

Do not put credentials directly in `.gitlab-ci.yml` or in a repository remote.
For SSH URLs, install the private key and known-host entries through GitLab's
file-type CI/CD variables and runner setup.

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
