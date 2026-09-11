#!/usr/bin/env bash
set -euo pipefail

readonly SCRIPT_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd -P)"
readonly REPOSITORY_ROOT="$(dirname "${SCRIPT_ROOT}")"
readonly BUILDINFO_PACKAGE='github.com/ZhuzhuNo3/transferlanes/internal/buildinfo'

architecture=''
output=''
release_version=''
development_commit=''
development_modified=''
release_set=0
commit_set=0
modified_set=0
linker_flags=''
staging=''

usage() {
  printf 'usage: %s --arch amd64|arm64 --output PATH (--release-version VERSION | --development-commit REVISION --development-modified true|false)\n' "$0" >&2
}

cleanup() {
  local status=$?
  trap - EXIT INT TERM HUP
  if [[ -n "${staging}" ]]; then
    rm -f -- "${staging}"
  fi
  exit "${status}"
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
      --release-version)
        [[ "${release_set}" -eq 0 && $# -ge 2 ]] || return 2
        release_version="$2"
        release_set=1
        shift 2
        ;;
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
  [[ "${architecture}" == amd64 || "${architecture}" == arm64 ]] || return 2
  [[ -n "${output}" ]] || return 2
  prepare_build_identity
}

prepare_build_identity() {
  if [[ "${release_set}" -eq 1 ]]; then
    [[ "${commit_set}" -eq 0 && "${modified_set}" -eq 0 ]] || return 2
    validate_release_version || return 2
    local encoded
    encoded="$(printf '%s' "${release_version}" | LC_ALL=C od -An -v -tx1 | tr -d '[:space:]')"
    [[ -n "${encoded}" ]] || return 2
    linker_flags="-X ${BUILDINFO_PACKAGE}.releaseVersionHex=${encoded}"
    return 0
  fi
  [[ "${commit_set}" -eq 1 && "${modified_set}" -eq 1 ]] || return 2
  [[ "${development_commit}" =~ ^([0-9a-f]{40}|[0-9a-f]{64})$ ]] || return 2
  [[ "${development_modified}" == true || "${development_modified}" == false ]] || return 2
  linker_flags="-X ${BUILDINFO_PACKAGE}.developmentCommit=${development_commit} -X ${BUILDINFO_PACKAGE}.developmentModified=${development_modified}"
}

validate_release_version() {
  [[ -n "${release_version}" ]] || return 1
  local LC_ALL=C index=0 length byte next following fourth
  length=${#release_version}
  while (( index < length )); do
    printf -v byte '%d' "'${release_version:index:1}"
    if (( byte >= 32 && byte <= 126 )); then
      index=$((index + 1))
      continue
    fi
    if (( byte <= 31 || byte == 127 || byte < 194 || byte > 244 )); then
      return 1
    fi
    if (( byte <= 223 )); then
      (( index + 1 < length )) || return 1
      printf -v next '%d' "'${release_version:index+1:1}"
      if (( next < 128 || next > 191 || byte == 194 && next <= 159 )); then
        return 1
      fi
      index=$((index + 2))
      continue
    fi
    if (( byte <= 239 )); then
      (( index + 2 < length )) || return 1
      printf -v next '%d' "'${release_version:index+1:1}"
      printf -v following '%d' "'${release_version:index+2:1}"
      if (( following < 128 || following > 191 ||
        byte == 224 && (next < 160 || next > 191) ||
        byte == 237 && (next < 128 || next > 159) ||
        byte != 224 && byte != 237 && (next < 128 || next > 191) ||
        byte == 226 && next == 128 && (following == 168 || following == 169) )); then
        return 1
      fi
      index=$((index + 3))
      continue
    fi
    (( index + 3 < length )) || return 1
    printf -v next '%d' "'${release_version:index+1:1}"
    printf -v following '%d' "'${release_version:index+2:1}"
    printf -v fourth '%d' "'${release_version:index+3:1}"
    if (( following < 128 || following > 191 || fourth < 128 || fourth > 191 ||
      byte == 240 && (next < 144 || next > 191) ||
      byte >= 241 && byte <= 243 && (next < 128 || next > 191) ||
      byte == 244 && (next < 128 || next > 143) )); then
      return 1
    fi
    index=$((index + 4))
  done
}

resolve_output() {
  [[ "${output}" != */ ]] || return 2
  local parent name
  parent="$(dirname -- "${output}")"
  name="$(basename -- "${output}")"
  [[ "${name}" != . && "${name}" != .. && "${name}" != - ]] || return 2
  [[ -d "${parent}" ]] || {
    printf 'build output parent does not exist: %s\n' "${parent}" >&2
    return 1
  }
  parent="$(cd "${parent}" && pwd -P)"
  output="${parent}/${name}"
  if [[ -e "${output}" || -L "${output}" ]]; then
    [[ -f "${output}" && ! -L "${output}" ]] || {
      printf 'build output is not a regular file: %s\n' "${output}" >&2
      return 1
    }
  fi
}

build() {
  staging="$(mktemp "$(dirname "${output}")/.transferlanes-build.XXXXXX")"
  (
    cd "${REPOSITORY_ROOT}"
    CGO_ENABLED=0 GOOS=linux GOARCH="${architecture}" \
      go build -trimpath -ldflags "${linker_flags}" -o "${staging}" ./cmd/transferlanes
  )
  chmod 0755 "${staging}"
  mv -f -- "${staging}" "${output}"
  staging=''
}

main() {
  if ! parse_arguments "$@"; then
    usage
    return 2
  fi
  resolve_output
  trap cleanup EXIT INT TERM HUP
  build
  trap - EXIT INT TERM HUP
}

main "$@"
