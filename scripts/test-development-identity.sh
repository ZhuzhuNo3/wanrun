#!/usr/bin/env bash
set -euo pipefail

readonly SCRIPT_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd -P)"
readonly COLLECTOR="${SCRIPT_ROOT}/private/development-identity.sh"

temp_root=''
fixture=''

cleanup() {
  if [[ -n "${temp_root}" && -d "${temp_root}" ]]; then
    rm -rf -- "${temp_root}"
  fi
}

fail() {
  printf 'development identity test failed: %s\n' "$1" >&2
  return 1
}

create_fixture() {
  local name="$1"
  fixture="${temp_root}/${name}"
  mkdir -p "${fixture}/cmd" "${fixture}/internal" "${fixture}/scripts/private"
  git -C "${fixture}" init -q --initial-branch=main
  git -C "${fixture}" config user.name 'Transfer Lanes Test'
  git -C "${fixture}" config user.email 'transferlanes@example.invalid'
  printf 'module example.invalid/fixture\n\ngo 1.25\n' >"${fixture}/go.mod"
  printf '%s\n' 'fixture checksum' >"${fixture}/go.sum"
  printf '%s\n' 'package main' >"${fixture}/cmd/main.go"
  printf '%s\n' 'package fixture' >"${fixture}/internal/core.go"
  printf '%s\n' '#!/usr/bin/env bash' >"${fixture}/scripts/build.sh"
  printf '%s\n' '#!/usr/bin/env bash' >"${fixture}/scripts/build-development.sh"
  cp -p -- "${COLLECTOR}" "${fixture}/scripts/private/development-identity.sh"
  printf '%s\n' 'internal/ignored.go' 'docs/' >"${fixture}/.gitignore"
  git -C "${fixture}" add .
  git -C "${fixture}" commit -qm 'fixture'
}

assert_identity() {
  local root="$1" modified="$2" actual="${temp_root}/actual" expected="${temp_root}/expected"
  local revision
  revision="$(git -C "${root}" rev-parse --verify HEAD^{commit})"
  : >"${temp_root}/collector-stderr"
  "${root}/scripts/private/development-identity.sh" "${root}" \
    >"${actual}" 2>"${temp_root}/collector-stderr" || fail "collector rejected ${root}"
  printf 'commit=%s\nmodified=%s\n' "${revision}" "${modified}" >"${expected}"
  cmp -s "${actual}" "${expected}" || fail "identity differs for ${root}"
  [[ ! -s "${temp_root}/collector-stderr" ]] || fail "collector wrote stderr on success"
}

test_modified_inputs() {
  create_fixture staged
  printf '%s\n' '// staged' >>"${fixture}/go.mod"
  git -C "${fixture}" add go.mod
  assert_identity "${fixture}" true

  create_fixture unstaged
  printf '%s\n' '// unstaged' >>"${fixture}/cmd/main.go"
  assert_identity "${fixture}" true

  create_fixture deleted
  rm -f -- "${fixture}/internal/core.go"
  assert_identity "${fixture}" true

  create_fixture untracked
  printf '%s\n' 'package fixture' >"${fixture}/internal/new.go"
  assert_identity "${fixture}" true

  create_fixture build_script
  printf '%s\n' '# build change' >>"${fixture}/scripts/build.sh"
  assert_identity "${fixture}" true

  create_fixture development_entry
  printf '%s\n' '# wrapper change' >>"${fixture}/scripts/build-development.sh"
  assert_identity "${fixture}" true

  create_fixture collector
  printf '%s\n' '# collector change' >>"${fixture}/scripts/private/development-identity.sh"
  assert_identity "${fixture}" true
}

test_excluded_inputs() {
  create_fixture excluded
  printf '%s\n' 'ignored' >"${fixture}/internal/ignored.go"
  mkdir -p "${fixture}/docs"
  printf '%s\n' 'ignored documentation' >"${fixture}/docs/note.md"
  printf '%s\n' 'binary output' >"${fixture}/transferlanes"
  printf '%s\n' '# outside build inputs' >"${fixture}/scripts/test-extra.sh"
  assert_identity "${fixture}" false
}

test_invalid_roots() {
  create_fixture valid
  if "${COLLECTOR}" "${fixture}/." >/dev/null 2>&1; then
    fail 'non-canonical repository root was accepted'
  fi
  mkdir "${temp_root}/not-a-repository"
  if "${COLLECTOR}" "${temp_root}/not-a-repository" >/dev/null 2>&1; then
    fail 'non-repository was accepted'
  fi
  mkdir "${temp_root}/no-head"
  git -C "${temp_root}/no-head" init -q --initial-branch=main
  if "${COLLECTOR}" "${temp_root}/no-head" >/dev/null 2>&1; then
    fail 'repository without HEAD was accepted'
  fi
}

main() {
  [[ -x "${COLLECTOR}" ]] || fail "collector is not executable: ${COLLECTOR}"
  temp_root="$(mktemp -d "${TMPDIR:-/tmp}/transferlanes-development-identity-test.XXXXXX")"
  temp_root="$(cd "${temp_root}" && pwd -P)"
  trap cleanup EXIT INT TERM HUP
  create_fixture clean
  assert_identity "${fixture}" false
  test_modified_inputs
  test_excluded_inputs
  test_invalid_roots
  cleanup
  trap - EXIT INT TERM HUP
  printf 'development identity tests passed\n'
}

main "$@"
