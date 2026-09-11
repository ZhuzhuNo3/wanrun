#!/usr/bin/env bash
set -euo pipefail

readonly SCRIPT_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd -P)"
readonly BUILD_SCRIPT="${SCRIPT_ROOT}/build.sh"
readonly DEVELOPMENT_BUILD_SCRIPT="${SCRIPT_ROOT}/build-development.sh"
readonly DEVELOPMENT_COMMIT='0123456789abcdef0123456789abcdef01234567'

temp_root=''

cleanup() {
  if [[ -n "${temp_root}" && -d "${temp_root}" ]]; then
    rm -rf -- "${temp_root}"
  fi
}

fail() {
  printf 'build test failed: %s\n' "$1" >&2
  return 1
}

expect_failure() {
  if "${BUILD_SCRIPT}" "$@" >/dev/null 2>&1; then
    fail "command unexpectedly succeeded: $*"
  fi
}

assert_linux_binary() {
  local binary="$1" arch="$2" description metadata
  [[ -f "${binary}" && -x "${binary}" ]] || fail "missing executable ${binary}"
  if command -v readelf >/dev/null 2>&1; then
    assert_readelf_binary "${binary}" "${arch}"
  else
    description="$(LC_ALL=C file -b "${binary}")"
    [[ "${description}" == ELF\ 64-bit* && "${description}" == *'statically linked'* ]] ||
      fail "${binary} is not a static ELF: ${description}"
    case "${arch}:${description}" in
      amd64:*x86-64*) ;;
      arm64:*aarch64*|arm64:*ARM64*) ;;
      *) fail "${binary} machine does not match ${arch}: ${description}" ;;
    esac
  fi
  metadata="$(go version -m "${binary}")"
  [[ "${metadata}" == *$'\tbuild\tGOOS=linux'* &&
    "${metadata}" == *$'\tbuild\tGOARCH='"${arch}"* &&
    "${metadata}" == *$'\tbuild\t-trimpath=true'* ]] ||
    fail "${binary} build metadata is incomplete: ${metadata}"
}

assert_readelf_binary() {
  local binary="$1" arch="$2" expected_machine header machine program_headers dynamic
  case "${arch}" in
    amd64) expected_machine='Advanced Micro Devices X86-64' ;;
    arm64) expected_machine='AArch64' ;;
  esac
  header="$(LC_ALL=C readelf -h "${binary}")"
  machine="$(printf '%s\n' "${header}" | sed -n \
    's/^[[:space:]]*Machine:[[:space:]]*//p')"
  [[ "${machine}" == "${expected_machine}" ]] ||
    fail "${binary} machine is ${machine}, want ${expected_machine}"
  program_headers="$(LC_ALL=C readelf -lW "${binary}")"
  if printf '%s\n' "${program_headers}" | grep -Eq '^[[:space:]]*INTERP[[:space:]]'; then
    fail "${binary} unexpectedly has an ELF interpreter"
  fi
  dynamic="$(LC_ALL=C readelf -dW "${binary}")"
  [[ "${dynamic}" != *'(NEEDED)'* ]] || fail "${binary} unexpectedly needs a shared library"
}

test_architectures_and_output_paths() {
  local relative_root="${temp_root}/relative"
  mkdir -p "${relative_root}"
  (
    cd "${relative_root}"
    "${BUILD_SCRIPT}" --arch amd64 --output transferlanes-amd64 --release-version v1.2.3
  )
  assert_linux_binary "${relative_root}/transferlanes-amd64" amd64
  "${BUILD_SCRIPT}" --arch arm64 --output "${temp_root}/transferlanes-arm64" \
    --development-commit "${DEVELOPMENT_COMMIT}" --development-modified false
  assert_linux_binary "${temp_root}/transferlanes-arm64" arm64
  if find "${temp_root}" -name '.transferlanes-build.*' -print -quit | grep -q .; then
    fail 'successful build left a staging artifact'
  fi
}

test_strict_arguments() {
  local output="${temp_root}/invalid"
  expect_failure
  expect_failure --arch amd64
  expect_failure --output "${output}"
  expect_failure --arch sparc --output "${output}" --release-version v1
  expect_failure --arch amd64 --arch arm64 --output "${output}" --release-version v1
  expect_failure --arch amd64 --output "${output}" --output "${output}.other" --release-version v1
  expect_failure --arch amd64 --output --release-version v1
  expect_failure --arch --output "${output}" --release-version v1
  expect_failure --unknown value --arch amd64 --output "${output}" --release-version v1
  mkdir "${temp_root}/directory-output"
  expect_failure --arch amd64 --output "${temp_root}/directory-output" --release-version v1
}

assert_version() {
  local binary="$1" expected="$2" actual="${temp_root}/version.actual"
  : >"${temp_root}/version.stderr"
  "${binary}" --version >"${actual}" 2>"${temp_root}/version.stderr" ||
    fail "${binary} rejected --version"
  printf '%s\n' "${expected}" >"${temp_root}/version.expected"
  cmp -s "${actual}" "${temp_root}/version.expected" || fail "${binary} version differs"
  [[ ! -s "${temp_root}/version.stderr" ]] || fail "${binary} wrote version diagnostics"
}

test_identity_outputs_on_linux() {
  [[ "$(uname -s)" == Linux ]] || return 0
  local native_arch
  case "$(uname -m)" in
    x86_64) native_arch=amd64 ;;
    aarch64) native_arch=arm64 ;;
    *) return 0 ;;
  esac
  local release_binary="${temp_root}/release-native"
  local development_binary="${temp_root}/development-native"
  local marker="${temp_root}/must-not-execute"
  local padding release
  printf -v padding '%0200d' 0
  release='--preview "quoted" \ path é版本🚀 '"${padding}"' $(touch '"${marker}"')'
  "${BUILD_SCRIPT}" --arch "${native_arch}" --output "${release_binary}" \
    --release-version "${release}"
  [[ ! -e "${marker}" ]] || fail 'release version executed shell content'
  assert_version "${release_binary}" "transferlanes ${release}"
  "${BUILD_SCRIPT}" --arch "${native_arch}" --output "${development_binary}" \
    --development-commit "${DEVELOPMENT_COMMIT}" --development-modified true
  assert_version "${development_binary}" \
    "transferlanes devel commit=${DEVELOPMENT_COMMIT} modified=true"
}

test_development_build_entry() {
  local architecture
  case "$(go env GOHOSTARCH)" in
    amd64|arm64) architecture="$(go env GOHOSTARCH)" ;;
    *) architecture=amd64 ;;
  esac
  local binary="${temp_root}/development-entry"
  "${DEVELOPMENT_BUILD_SCRIPT}" --arch "${architecture}" --output "${binary}"
  assert_linux_binary "${binary}" "${architecture}"
  if [[ "$(uname -s)" == Linux ]] && {
    [[ "${architecture}" == amd64 && "$(uname -m)" == x86_64 ]] ||
      [[ "${architecture}" == arm64 && "$(uname -m)" == aarch64 ]];
  }; then
    local identity commit modified
    identity="$("${SCRIPT_ROOT}/private/development-identity.sh" \
      "$(cd "${SCRIPT_ROOT}/.." && pwd -P)")"
    commit="$(printf '%s\n' "${identity}" | sed -n 's/^commit=//p')"
    modified="$(printf '%s\n' "${identity}" | sed -n 's/^modified=//p')"
    assert_version "${binary}" "transferlanes devel commit=${commit} modified=${modified}"
  fi
}

test_identity_is_rejected_before_compile() {
  local fake_bin="${temp_root}/identity-fake-bin" marker="${temp_root}/compiler-called"
  local output="${temp_root}/invalid-identity"
  mkdir "${fake_bin}"
  printf '%s\n' '#!/usr/bin/env bash' 'touch "$TRANSFERLANES_FAKE_GO_MARKER"' 'exit 0' >"${fake_bin}/go"
  chmod 0755 "${fake_bin}/go"
  local -a prefix=(env "TRANSFERLANES_FAKE_GO_MARKER=${marker}" "PATH=${fake_bin}:${PATH}" \
    "${BUILD_SCRIPT}" --arch amd64 --output "${output}")
  expect_identity_failure() {
    rm -f -- "${marker}" "${output}"
    if "${prefix[@]}" "$@" >/dev/null 2>&1; then
      fail "invalid identity unexpectedly succeeded: $*"
    fi
    [[ ! -e "${marker}" ]] || fail "invalid identity invoked the Go compiler: $*"
  }
  expect_identity_failure
  expect_identity_failure --release-version ''
  expect_identity_failure --release-version $'line\nbreak'
  expect_identity_failure --release-version $'control\177'
  expect_identity_failure --release-version $'c1\302\200'
  expect_identity_failure --release-version $'separator\342\200\250'
  expect_identity_failure --release-version $'paragraph\342\200\251'
  expect_identity_failure --release-version $'invalid\377'
  expect_identity_failure --release-version $'overlong\300\200'
  expect_identity_failure --release-version $'surrogate\355\240\200'
  expect_identity_failure --release-version $'too-high\364\220\200\200'
  expect_identity_failure --release-version v1 --development-commit "${DEVELOPMENT_COMMIT}" \
    --development-modified false
  expect_identity_failure --development-commit 0123456 --development-modified false
  expect_identity_failure --development-commit "${DEVELOPMENT_COMMIT}"
  expect_identity_failure --development-commit "${DEVELOPMENT_COMMIT}" --development-modified yes
  unset -f expect_identity_failure
}

test_failed_build_preserves_output_and_removes_staging() {
  local fake_bin="${temp_root}/fake-bin" output="${temp_root}/preserved"
  local absent_output="${temp_root}/must-remain-absent"
  mkdir "${fake_bin}"
  printf '%s\n' '#!/usr/bin/env bash' 'for ((i=1; i<=$#; i++)); do' \
    '  if [[ ${!i} == -o ]]; then j=$((i + 1)); printf partial >"${!j}"; fi' \
    'done' 'exit 47' >"${fake_bin}/go"
  chmod 0755 "${fake_bin}/go"
  printf 'original\n' >"${output}"
  if PATH="${fake_bin}:${PATH}" "${BUILD_SCRIPT}" --arch amd64 --output "${output}" \
    --release-version controlled-failure; then
    fail 'controlled compiler failure unexpectedly succeeded'
  fi
  [[ "$(cat "${output}")" == original ]] || fail 'failed build replaced the existing output'
  if PATH="${fake_bin}:${PATH}" "${BUILD_SCRIPT}" --arch amd64 --output "${absent_output}" \
    --release-version controlled-failure; then
    fail 'second controlled compiler failure unexpectedly succeeded'
  fi
  [[ ! -e "${absent_output}" && ! -L "${absent_output}" ]] ||
    fail 'failed build created the requested output'
  if find "${temp_root}" -name '.transferlanes-build.*' -print -quit | grep -q .; then
    fail 'failed build left a staging artifact'
  fi
}

main() {
  temp_root="$(mktemp -d "${TMPDIR:-/tmp}/transferlanes-build-test.XXXXXX")"
  trap cleanup EXIT INT TERM HUP
  test_architectures_and_output_paths
  test_strict_arguments
  test_identity_is_rejected_before_compile
  test_identity_outputs_on_linux
  if [[ -e "${SCRIPT_ROOT}/../.git" ]]; then
    test_development_build_entry
  fi
  test_failed_build_preserves_output_and_removes_staging
  if grep -Eq '(^|[^[:alnum:]_])git([[:space:]]|$)' "${BUILD_SCRIPT}"; then
    fail 'build script performs Git discovery'
  fi
  cleanup
  trap - EXIT INT TERM HUP
  printf 'build tests passed\n'
}

main "$@"
