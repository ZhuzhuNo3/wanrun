#!/usr/bin/env bash
set -euo pipefail

readonly SCRIPT_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd -P)"
readonly CONTEXT_HELPER="${SCRIPT_ROOT}/private/root-build-context.sh"

temp_root=''
development_commit=''
development_modified=''
commit_set=0
modified_set=0

cleanup_fixture() {
  if [[ -n "${temp_root}" && -d "${temp_root}" ]]; then
    rm -rf -- "${temp_root}"
  fi
}

exit_for_signal() {
  local status="$1"
  trap - INT TERM HUP
  exit "${status}"
}

write_fixture() {
  local repo="$1"
  mkdir -p "${repo}/cmd/transferlanes" "${repo}/internal/example" "${repo}/scripts/private" "${repo}/secrets"
  printf 'module github.com/ZhuzhuNo3/transferlanes\n\ngo 1.25\n' >"${repo}/go.mod"
  : >"${repo}/go.sum"
  printf '%s\n' 'package main' '' 'import (' \
    '  "fmt"' '  "os"' '  "github.com/ZhuzhuNo3/transferlanes/internal/buildinfo"' ')' '' \
    'func main() {' '  if len(os.Args) != 2 || os.Args[1] != "--version" { os.Exit(1) }' \
    '  text, err := buildinfo.Text()' '  if err != nil { os.Exit(1) }' '  fmt.Print(text)' '}' \
    >"${repo}/cmd/transferlanes/main.go"
  printf 'package example\n' >"${repo}/internal/example/untracked.go"
  mkdir -p "${repo}/internal/buildinfo"
  cp -pP -- "${SCRIPT_ROOT}/../internal/buildinfo/buildinfo.go" \
    "${repo}/internal/buildinfo/buildinfo.go"
  cp -pP -- "${SCRIPT_ROOT}/build.sh" "${repo}/scripts/build.sh"
  cp -pP -- "${SCRIPT_ROOT}/private/install-root-test-dependencies.sh" \
    "${repo}/scripts/private/install-root-test-dependencies.sh"
  printf '#!/usr/bin/env bash\nexit 99\n' >"${repo}/scripts/not-allowed.sh"
  chmod 0755 "${repo}/scripts/build.sh" "${repo}/scripts/private/install-root-test-dependencies.sh" \
    "${repo}/scripts/not-allowed.sh"
  printf '.env\ninternal/example/ignored.secret\nscripts/ignored.secret\n' >"${repo}/.gitignore"
  printf 'secret\n' >"${repo}/.env"
  printf 'ignored\n' >"${repo}/internal/example/ignored.secret"
  printf 'ignored\n' >"${repo}/scripts/ignored.secret"
  printf 'tracked secret\n' >"${repo}/secrets/production.key"
  git -C "${repo}" init -q
  git -C "${repo}" add go.mod go.sum .gitignore cmd/transferlanes/main.go internal/buildinfo/buildinfo.go \
    scripts/build.sh scripts/private/install-root-test-dependencies.sh \
    scripts/not-allowed.sh secrets/production.key
}

assert_regular() {
  local path="$1"
  [[ -f "${path}" && ! -L "${path}" ]] || {
    printf 'expected copied regular file: %s\n' "${path}" >&2
    return 1
  }
}

assert_absent() {
  local path="$1"
  [[ ! -e "${path}" && ! -L "${path}" ]] || {
    printf 'unexpected build-context entry: %s\n' "${path}" >&2
    return 1
  }
}

test_allowlist_copy() {
  local repo="${temp_root}/repo"
  local context="${temp_root}/context"
  mkdir -p "${repo}"
  write_fixture "${repo}"
  "${CONTEXT_HELPER}" "${repo}" "${context}"
  assert_regular "${context}/go.mod"
  assert_regular "${context}/go.sum"
  assert_regular "${context}/cmd/transferlanes/main.go"
  assert_regular "${context}/internal/example/untracked.go"
  assert_regular "${context}/internal/buildinfo/buildinfo.go"
  assert_regular "${context}/scripts/build.sh"
  assert_regular "${context}/scripts/private/install-root-test-dependencies.sh"
  assert_absent "${context}/.env"
  assert_absent "${context}/internal/example/ignored.secret"
  assert_absent "${context}/scripts/ignored.secret"
  assert_absent "${context}/scripts/not-allowed.sh"
  assert_absent "${context}/secrets/production.key"
  assert_absent "${context}/.git"
  "${context}/scripts/build.sh" --arch amd64 --output "${temp_root}/context-transferlanes" \
    --development-commit "${development_commit}" \
    --development-modified "${development_modified}"
  assert_regular "${temp_root}/context-transferlanes"
  if [[ "$(uname -s)" == Linux && "$(uname -m)" == x86_64 ]]; then
    printf 'transferlanes devel commit=%s modified=%s\n' \
      "${development_commit}" "${development_modified}" >"${temp_root}/version.expected"
    "${temp_root}/context-transferlanes" --version >"${temp_root}/version.actual"
    cmp -s "${temp_root}/version.actual" "${temp_root}/version.expected"
  fi
}

test_symlink_escape_rejected() {
  local repo="${temp_root}/symlink-repo"
  local context="${temp_root}/symlink-context"
  mkdir -p "${repo}"
  write_fixture "${repo}"
  printf 'outside\n' >"${temp_root}/outside.go"
  ln -s "${temp_root}/outside.go" "${repo}/internal/example/escape.go"
  if "${CONTEXT_HELPER}" "${repo}" "${context}"; then
    printf 'symlink escape unexpectedly entered build context\n' >&2
    return 1
  fi
  assert_absent "${context}"
}

test_parent_symlink_escape_rejected() {
  local repo="${temp_root}/parent-symlink-repo"
  local context="${temp_root}/parent-symlink-context"
  local outside="${temp_root}/outside-parent"
  mkdir -p "${repo}"
  write_fixture "${repo}"
  mkdir -p "${repo}/internal/cached" "${outside}"
  printf 'package cached\nvar source = "tracked"\n' >"${repo}/internal/cached/nested.go"
  git -C "${repo}" add internal/cached/nested.go
  unlink "${repo}/internal/cached/nested.go"
  rmdir "${repo}/internal/cached"
  printf 'package cached\nvar source = "outside"\n' >"${outside}/nested.go"
  cp -pP -- "${outside}/nested.go" "${temp_root}/outside-baseline.go"
  printf 'internal/cached\n' >"${repo}/.git/info/exclude"
  ln -s "${outside}" "${repo}/internal/cached"
  if "${CONTEXT_HELPER}" "${repo}" "${context}"; then
    if cmp -s "${outside}/nested.go" "${context}/internal/cached/nested.go"; then
      printf 'parent symlink copied outside bytes into build context\n' >&2
    fi
    return 1
  fi
  cmp -s "${temp_root}/outside-baseline.go" "${outside}/nested.go"
  assert_absent "${context}"
}

parse_identity_arguments() {
  while (( $# != 0 )); do
    case "$1" in
      --development-commit)
        [[ "${commit_set}" -eq 0 && $# -ge 2 ]] || return 2
        development_commit="$2"
        commit_set=1
        shift 2
        ;;
      --development-modified)
        [[ "${modified_set}" -eq 0 && $# -ge 2 ]] || return 2
        development_modified="$2"
        modified_set=1
        shift 2
        ;;
      *) return 2 ;;
    esac
  done
  validate_identity || return 2
}

validate_identity() {
  [[ "${development_commit}" =~ ^([0-9a-f]{40}|[0-9a-f]{64})$ ]] || return 1
  [[ "${development_modified}" == true || "${development_modified}" == false ]] || return 1
}

collect_identity() {
  local identity first second rest
  identity="$("${SCRIPT_ROOT}/private/development-identity.sh" "$(cd "${SCRIPT_ROOT}/.." && pwd -P)")" || return 1
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
  if (( $# == 0 )); then
    collect_identity || {
      printf 'secure context test: cannot collect development identity\n' >&2
      return 1
    }
  elif ! parse_identity_arguments "$@"; then
    printf 'usage: %s [--development-commit REVISION --development-modified true|false]\n' "$0" >&2
    return 2
  fi
  local created_root
  created_root="$(mktemp -d "${TMPDIR:-/tmp}/transferlanes-context-test.XXXXXX")"
  temp_root="$(cd "${created_root}" && pwd -P)"
  trap cleanup_fixture EXIT
  trap 'exit_for_signal 130' INT
  trap 'exit_for_signal 143' TERM
  trap 'exit_for_signal 129' HUP
  test_allowlist_copy
  test_symlink_escape_rejected
  test_parent_symlink_escape_rejected
  cleanup_fixture
  trap - EXIT INT TERM HUP
  printf 'secure root build context tests passed\n'
}

main "$@"
