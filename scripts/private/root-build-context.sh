#!/usr/bin/env bash
set -euo pipefail

repo_root=''
destination=''
manifest=''
created=0

cleanup_context() {
  local status=$?
  trap - EXIT INT TERM HUP
  if [[ -n "${manifest}" ]]; then
    rm -f -- "${manifest}"
  fi
  if [[ "${status}" -ne 0 && "${created}" -eq 1 ]]; then
    rm -rf -- "${destination}"
  fi
  exit "${status}"
}

exit_for_signal() {
  local status="$1"
  trap - INT TERM HUP
  exit "${status}"
}

validate_roots() {
  [[ "$#" -eq 2 ]] || { printf 'usage: %s REPO_ROOT CONTEXT\n' "$0" >&2; return 2; }
  repo_root="$1"
  destination="$2"
  [[ "${repo_root}" = /* && "${destination}" = /* ]] || {
    printf 'repository and context paths must be absolute\n' >&2
    return 1
  }
  local canonical_repo git_root parent candidate
  canonical_repo="$(cd "${repo_root}" && pwd -P)"
  git_root="$(git -C "${repo_root}" rev-parse --show-toplevel)"
  git_root="$(cd "${git_root}" && pwd -P)"
  parent="$(cd "$(dirname "${destination}")" && pwd -P)"
  candidate="${parent}/$(basename "${destination}")"
  [[ "${repo_root}" == "${canonical_repo}" && "${repo_root}" == "${git_root}" &&
    "${destination}" == "${candidate}" ]] || {
    printf 'repository and context paths must be canonical\n' >&2
    return 1
  }
  [[ ! -e "${destination}" && ! -L "${destination}" ]] || {
    printf 'repository must be Git-controlled and context must not exist\n' >&2
    return 1
  }
}

validate_relative_path() {
  local relative="$1"
  case "${relative}" in
    go.mod|go.sum|scripts/build.sh|scripts/private/install-root-test-dependencies.sh|cmd/*|internal/*) ;;
    *) printf 'refusing unexpected context path: %q\n' "${relative}" >&2; return 1 ;;
  esac
  case "/${relative}/" in
    *'/../'*|*'/./'*|*'//'*)
      printf 'refusing non-canonical context path: %q\n' "${relative}" >&2
      return 1
      ;;
  esac
}

validate_source_path() {
  local relative="$1"
  local source="${repo_root}/${relative}"
  local relative_parent expected_parent physical_parent
  relative_parent="$(dirname "${relative}")"
  expected_parent="${repo_root}"
  if [[ "${relative_parent}" != '.' ]]; then
    expected_parent="${repo_root}/${relative_parent}"
  fi
  physical_parent="$(cd -- "$(dirname "${source}")" && pwd -P)"
  [[ "${physical_parent}" == "${expected_parent}" ]] || {
    printf 'context input traverses a symlinked parent: %q\n' "${relative}" >&2
    return 1
  }
  [[ -f "${source}" && ! -L "${source}" ]] || {
    printf 'context input is not a regular non-symlink file: %q\n' "${relative}" >&2
    return 1
  }
}

validate_copied_file() {
  local relative="$1" source="$2" target="$3"
  validate_source_path "${relative}"
  [[ -f "${target}" && ! -L "${target}" ]] || {
    printf 'copied context entry changed kind: %q\n' "${relative}" >&2
    return 1
  }
  cmp -s "${source}" "${target}" || {
    printf 'context input changed while copying: %q\n' "${relative}" >&2
    return 1
  }
}

copy_manifest() {
  local count=0 relative source target
  while IFS= read -r -d '' relative; do
    validate_relative_path "${relative}"
    source="${repo_root}/${relative}"
    target="${destination}/${relative}"
    validate_source_path "${relative}"
    mkdir -p -- "$(dirname "${target}")"
    cp -pP -- "${source}" "${target}"
    validate_copied_file "${relative}" "${source}" "${target}"
    count=$((count + 1))
  done <"${manifest}"
  [[ "${count}" -gt 0 ]] || { printf 'empty root build context\n' >&2; return 1; }
  for relative in go.mod go.sum scripts/private/install-root-test-dependencies.sh; do
    [[ -f "${destination}/${relative}" && ! -L "${destination}/${relative}" ]] || {
      printf 'required context input missing: %s\n' "${relative}" >&2
      return 1
    }
  done
  [[ -x "${destination}/scripts/build.sh" && ! -L "${destination}/scripts/build.sh" ]] || {
    printf 'required context build script missing or not executable\n' >&2
    return 1
  }
}

main() {
  validate_roots "$@"
  trap cleanup_context EXIT
  trap 'exit_for_signal 130' INT
  trap 'exit_for_signal 143' TERM
  trap 'exit_for_signal 129' HUP
  manifest="$(mktemp "$(dirname "${destination}")/.transferlanes-context.XXXXXX")"
  git -C "${repo_root}" ls-files --cached --others --exclude-standard -z -- \
    go.mod go.sum scripts/build.sh scripts/private/install-root-test-dependencies.sh \
    cmd internal >"${manifest}"
  mkdir -m 0700 -- "${destination}"
  created=1
  copy_manifest
}

main "$@"
