#!/usr/bin/env bash
set -euo pipefail

readonly SCRIPT_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd -P)"
readonly REPOSITORY_ROOT="$(dirname "${SCRIPT_ROOT}")"
readonly COLLECTOR="${SCRIPT_ROOT}/private/development-identity.sh"
readonly BUILD_SCRIPT="${SCRIPT_ROOT}/build.sh"

architecture=''
output=''

usage() {
  printf 'usage: %s --arch amd64|arm64 --output PATH\n' "$0" >&2
}

parse_arguments() {
  while (( $# != 0 )); do
    case "$1" in
      --arch)
        [[ -z "${architecture}" && $# -ge 2 && "$2" != --* ]] || return 2
        architecture="$2"
        shift 2
        ;;
      --output)
        [[ -z "${output}" && $# -ge 2 && "$2" != --* ]] || return 2
        output="$2"
        shift 2
        ;;
      *) return 2 ;;
    esac
  done
  [[ "${architecture}" == amd64 || "${architecture}" == arm64 ]] || return 2
  [[ -n "${output}" ]] || return 2
}

read_identity() {
  local identity first second rest
  identity="$("${COLLECTOR}" "${REPOSITORY_ROOT}")" || return 1
  first="${identity%%$'\n'*}"
  [[ "${identity}" != "${first}" ]] || return 1
  rest="${identity#*$'\n'}"
  second="${rest%%$'\n'*}"
  [[ "${rest}" == "${second}" ]] || return 1
  development_commit="${first#commit=}"
  development_modified="${second#modified=}"
  [[ "${first}" == "commit=${development_commit}" &&
    "${development_commit}" =~ ^([0-9a-f]{40}|[0-9a-f]{64})$ ]] || return 1
  [[ "${second}" == "modified=${development_modified}" &&
    ( "${development_modified}" == true || "${development_modified}" == false ) ]] || return 1
}

main() {
  if ! parse_arguments "$@"; then
    usage
    return 2
  fi
  local development_commit development_modified
  read_identity || {
    printf 'development build: cannot read development identity\n' >&2
    return 1
  }
  "${BUILD_SCRIPT}" --arch "${architecture}" --output "${output}" \
    --development-commit "${development_commit}" \
    --development-modified "${development_modified}"
}

main "$@"
