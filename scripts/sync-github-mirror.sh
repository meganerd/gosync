#!/bin/sh
set -eu

mode=${1:-}

case "$mode" in
  push)
    : "${GITHUB_MIRROR_URL:?GITHUB_MIRROR_URL is required}"
    git push "$GITHUB_MIRROR_URL" \
      "refs/heads/${CI_DEFAULT_BRANCH:-main}:refs/heads/${CI_DEFAULT_BRANCH:-main}" \
      'refs/tags/*:refs/tags/*'
    ;;

  pull-requests)
    : "${GITHUB_MIRROR_URL:?GITHUB_MIRROR_URL is required}"
    : "${GITLAB_MIRROR_PUSH_URL:?GITLAB_MIRROR_PUSH_URL is required}"

    # GitHub exposes every open pull request, including contributions from
    # forks, through refs/pull/<number>/head. Import each one under a namespace
    # that cannot update GitLab's authoritative branches.
    git ls-remote "$GITHUB_MIRROR_URL" 'refs/pull/*/head' |
      while read -r object_id ref_name; do
        pr_number=${ref_name#refs/pull/}
        pr_number=${pr_number%/head}
        git fetch --no-tags "$GITHUB_MIRROR_URL" "$ref_name"
        git push "$GITLAB_MIRROR_PUSH_URL" \
          "${object_id}:refs/heads/github-pr/${pr_number}"
      done
    ;;

  *)
    echo "usage: $0 {push|pull-requests}" >&2
    exit 2
    ;;
esac
