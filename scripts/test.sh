#!/usr/bin/env bash
set -euo pipefail

repo_root=''
temp_base=''
temp_dir=''
image_name=''
container_name=''
native_platform=''
owns_image=0
owns_container=0
development_commit=''
development_modified=''
provided_image=''
commit_set=0
modified_set=0
image_set=0

cleanup_named_objects() {
  local failed=0
  if [[ "${owns_container}" -eq 1 ]] && \
    docker container inspect "${container_name}" >/dev/null 2>&1; then
    docker container rm -f "${container_name}" >/dev/null || failed=1
  fi
  if [[ "${owns_image}" -eq 1 ]] && docker image inspect "${image_name}" >/dev/null 2>&1; then
    docker image rm -f "${image_name}" >/dev/null || failed=1
  fi
  if [[ -n "${temp_dir}" && -d "${temp_dir}" ]]; then
    local parent name suffix
    parent="$(dirname "${temp_dir}")"
    name="$(basename "${temp_dir}")"
    suffix="${name#transferlanes-default.}"
    if [[ "${parent}" == "${temp_base}" && -n "${suffix}" && "${suffix}" != "${name}" ]]; then
      rm -rf -- "${temp_dir}" || failed=1
    else
      printf 'refusing unsafe default-test cleanup: %s\n' "${temp_dir}" >&2
      failed=1
    fi
  fi
  return "${failed}"
}

cleanup_on_exit() {
  local primary_status=$?
  trap - EXIT INT TERM HUP
  local cleanup_status=0
  cleanup_and_verify_absence || cleanup_status=$?
  if [[ "${primary_status}" -eq 0 ]]; then
    primary_status="${cleanup_status}"
  fi
  exit "${primary_status}"
}

cleanup_and_verify_absence() {
  local cleanup_status=0
  cleanup_named_objects || cleanup_status=$?
  if [[ "${cleanup_status}" -ne 0 ]]; then
    printf 'default-test cleanup failed\n' >&2
  fi
  local absence_status=0
  verify_absence || absence_status=$?
  if [[ "${cleanup_status}" -ne 0 || "${absence_status}" -ne 0 ]]; then
    return 1
  fi
}

exit_for_signal() {
  local status="$1"
  trap - INT TERM HUP
  exit "${status}"
}

initialize_names() {
  repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
  temp_base="$(cd "${TMPDIR:-/tmp}" && pwd -P)"
  temp_dir="$(mktemp -d "${temp_base}/transferlanes-default.XXXXXX")"
  trap cleanup_on_exit EXIT
  trap 'exit_for_signal 130' INT
  trap 'exit_for_signal 143' TERM
  trap 'exit_for_signal 129' HUP
  local suffix
  suffix="$(basename "${temp_dir}")"
  suffix="${suffix#transferlanes-default.}"
  suffix="$(printf '%s' "${suffix}" | tr '[:upper:]' '[:lower:]')"
  container_name="transferlanes-default-${suffix}"
  if docker container inspect "${container_name}" >/dev/null 2>&1; then
    printf 'generated default-test Docker name already exists\n' >&2
    return 1
  fi
  owns_container=1
  if [[ -n "${provided_image}" ]]; then
    image_name="${provided_image}"
    if ! docker image inspect "${image_name}" >/dev/null 2>&1; then
      printf 'provided default-test image does not exist: %s\n' "${image_name}" >&2
      return 1
    fi
    return
  fi
  image_name="transferlanes-default-${suffix}:acceptance"
  if docker image inspect "${image_name}" >/dev/null 2>&1; then
    printf 'generated default-test Docker name already exists\n' >&2
    return 1
  fi
  owns_image=1
}

detect_daemon_platform() {
  local daemon_platform
  if ! daemon_platform="$(docker version --format '{{.Server.Os}}/{{.Server.Arch}}')"; then
    printf 'transferlanes default test: cannot determine Docker daemon platform\n' >&2
    return 1
  fi
  case "${daemon_platform}" in
    linux/amd64|linux/arm64) native_platform="${daemon_platform}" ;;
    *)
      printf 'transferlanes default test: unsupported Docker daemon platform %q\n' \
        "${daemon_platform}" >&2
      return 1
      ;;
  esac
}

verify_absence() {
  local failed=0
  if docker container inspect "${container_name}" >/dev/null 2>&1; then
    printf 'default-test container remains: %s\n' "${container_name}" >&2
    failed=1
  fi
  if [[ "${owns_image}" -eq 1 ]] && \
    docker image inspect "${image_name}" >/dev/null 2>&1; then
    printf 'default-test image remains: %s\n' "${image_name}" >&2
    failed=1
  fi
  if [[ -e "${temp_dir}" ]]; then
    printf 'default-test temporary directory remains: %s\n' "${temp_dir}" >&2
    failed=1
  fi
  return "${failed}"
}

run_default_container() {
  local status=0
  printf 'transferlanes default test: native platform %s\n' "${native_platform}"
  if [[ "${owns_image}" -eq 1 ]]; then
    docker build --platform "${native_platform}" --file "${repo_root}/test/default/Dockerfile" \
      --build-arg "TRANSFERLANES_DEVELOPMENT_COMMIT=${development_commit}" \
      --build-arg "TRANSFERLANES_DEVELOPMENT_MODIFIED=${development_modified}" \
      --tag "${image_name}" "${repo_root}" || status=$?
    if [[ "${status}" -ne 0 ]]; then
      return "${status}"
    fi
  else
    printf 'transferlanes default test: using image %s\n' "${image_name}"
  fi
  docker run --rm --privileged --platform "${native_platform}" \
    --name "${container_name}" "${image_name}" || status=$?
  return "${status}"
}

parse_identity_arguments() {
  while (( $# != 0 )); do
    case "$1" in
      --development-commit)
        [[ "${commit_set}" -eq 0 && $# -ge 2 ]] || return 64
        development_commit="$2"
        commit_set=1
        shift 2
        ;;
      --development-modified)
        [[ "${modified_set}" -eq 0 && $# -ge 2 ]] || return 64
        development_modified="$2"
        modified_set=1
        shift 2
        ;;
      --image)
        [[ "${image_set}" -eq 0 && $# -ge 2 && -n "$2" ]] || return 64
        provided_image="$2"
        image_set=1
        shift 2
        ;;
      *) return 64 ;;
    esac
  done
  validate_identity || return 64
}

validate_identity() {
  [[ "${development_commit}" =~ ^([0-9a-f]{40}|[0-9a-f]{64})$ ]] || return 1
  [[ "${development_modified}" == true || "${development_modified}" == false ]] || return 1
}

collect_identity() {
  local identity first second rest
  identity="$("${repo_root}/scripts/private/development-identity.sh" "${repo_root}")" || return 1
  first="${identity%%$'\n'*}"
  [[ "${identity}" != "${first}" ]] || return 1
  rest="${identity#*$'\n'}"
  second="${rest%%$'\n'*}"
  [[ "${rest}" == "${second}" ]] || return 1
  development_commit="${first#commit=}"
  development_modified="${second#modified=}"
  [[ "${first}" == "commit=${development_commit}" &&
    "${second}" == "modified=${development_modified}" ]] || return 1
  validate_identity
}

main() {
  repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
  if (( $# == 0 )); then
    collect_identity || {
      printf 'transferlanes default test: cannot collect development identity\n' >&2
      return 1
    }
  elif ! parse_identity_arguments "$@"; then
    printf 'usage: scripts/test.sh [--development-commit REVISION --development-modified true|false] [--image IMAGE]\n' >&2
    return 64
  fi
  if ! command -v docker >/dev/null 2>&1; then
    printf 'transferlanes default test: Docker is required but was not found\n' >&2
    return 127
  fi
  detect_daemon_platform
  initialize_names
  local status=0
  run_default_container || status=$?
  local cleanup_status=0
  cleanup_and_verify_absence || cleanup_status=$?
  if [[ "${status}" -eq 0 ]]; then
    status="${cleanup_status}"
  fi
  trap - EXIT INT TERM HUP
  return "${status}"
}

main "$@"
