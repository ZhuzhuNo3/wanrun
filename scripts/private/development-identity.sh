#!/usr/bin/env bash
set -euo pipefail

repository_root=''

fail() {
  printf 'development identity: %s\n' "$1" >&2
  return 1
}

validate_repository_root() {
  [[ "$#" -eq 1 ]] || {
    printf 'usage: %s CANONICAL_REPOSITORY_ROOT\n' "$0" >&2
    return 2
  }
  repository_root="$1"
  [[ "${repository_root}" = /* && -d "${repository_root}" ]] || {
    fail 'repository root must be an existing absolute directory'
    return 1
  }
  local canonical git_root
  canonical="$(cd "${repository_root}" && pwd -P)"
  [[ "${repository_root}" == "${canonical}" ]] || {
    fail 'repository root is not canonical'
    return 1
  }
  git_root="$(git -C "${repository_root}" rev-parse --show-toplevel 2>/dev/null)" || {
    fail 'repository root is not Git-controlled'
    return 1
  }
  git_root="$(cd "${git_root}" && pwd -P)"
  [[ "${git_root}" == "${repository_root}" ]] || {
    fail 'path is not the repository root'
    return 1
  }
}

read_revision() {
  local revision
  revision="$(git -C "${repository_root}" rev-parse --verify HEAD^{commit} 2>/dev/null)" || {
    fail 'repository has no readable HEAD commit'
    return 1
  }
  [[ "${revision}" =~ ^([0-9a-f]{40}|[0-9a-f]{64})$ ]] || {
    fail 'HEAD is not a full lowercase hexadecimal revision'
    return 1
  }
  printf '%s\n' "${revision}"
}

read_modified() {
  local changes
  changes="$(git -C "${repository_root}" status --porcelain=v1 --untracked-files=all -- \
    go.mod go.sum cmd internal scripts/build.sh scripts/build-development.sh \
    scripts/private/development-identity.sh)" || {
    fail 'cannot inspect build inputs'
    return 1
  }
  if [[ -n "${changes}" ]]; then
    printf 'true\n'
  else
    printf 'false\n'
  fi
}

main() {
  validate_repository_root "$@" || return $?
  local revision modified
  revision="$(read_revision)" || return $?
  modified="$(read_modified)" || return $?
  printf 'commit=%s\nmodified=%s\n' "${revision}" "${modified}"
}

main "$@"
