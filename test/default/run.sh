#!/usr/bin/env bash
set -euo pipefail

readonly WORKSPACE='/workspace'
readonly ROOT_TEST_TIMEOUT='20m'
readonly PRODUCT_PACKAGE_PATTERNS=('./cmd/...' './internal/...')

run_root=''
development_commit=''
development_modified=''
special_packages=()
declare -A ordinary_targets=()
declare -A special_targets=()

cleanup_run() {
  local status=$?
  trap - EXIT INT TERM HUP
  if [[ -n "${run_root}" && -d "${run_root}" ]]; then
    local base name suffix
    base="$(dirname "${run_root}")"
    name="$(basename "${run_root}")"
    suffix="${name#transferlanes-default-run.}"
    if [[ "${base}" == '/tmp' && -n "${suffix}" && "${suffix}" != "${name}" ]]; then
      rm -rf -- "${run_root}"
    else
      printf 'refusing unsafe inner cleanup: %s\n' "${run_root}" >&2
      status=1
    fi
  fi
  exit "${status}"
}

exit_for_signal() {
  local status="$1"
  trap - INT TERM HUP
  exit "${status}"
}

initialize_run() {
  cd "${WORKSPACE}"
  run_root="$(mktemp -d /tmp/transferlanes-default-run.XXXXXX)"
  chmod 0755 "${run_root}"
  trap cleanup_run EXIT
  trap 'exit_for_signal 130' INT
  trap 'exit_for_signal 143' TERM
  trap 'exit_for_signal 129' HUP
  [[ ! -e "${WORKSPACE}/.git" ]]
  development_commit="${TRANSFERLANES_DEVELOPMENT_COMMIT:-}"
  development_modified="${TRANSFERLANES_DEVELOPMENT_MODIFIED:-}"
  [[ "${development_commit}" =~ ^([0-9a-f]{40}|[0-9a-f]{64})$ ]]
  [[ "${development_modified}" == true || "${development_modified}" == false ]]
}

print_versions_and_packages() {
  go version
  printf 'Go host: %s/%s; kernel: %s/%s\n' \
    "$(go env GOHOSTOS)" "$(go env GOHOSTARCH)" "$(uname -s)" "$(uname -m)"
  rsync --version
  ssh -V 2>&1
  sshd -V 2>&1
  aws --version
  minio --version
  ip -Version
  iptables --version
  nft --version
  bindfs --version
  fusermount --version
  dpkg-query -W -f='${Package}=${Version}\n' \
    rsync openssh-client openssh-server iproute2 iptables nftables util-linux procps \
    bindfs fuse ca-certificates
}

verify_format_and_modules() {
  local format_list
  format_list="$(find cmd internal test -type f -name '*.go' -print0 | \
    sort -z | xargs -0 gofmt -l)"
  if [[ -n "${format_list}" ]]; then
    printf 'gofmt required:\n%s\n' "${format_list}" >&2
    return 1
  fi
  cp go.mod "${run_root}/go.mod.saved"
  cp go.sum "${run_root}/go.sum.saved"
  go mod tidy -diff
  cmp -s go.mod "${run_root}/go.mod.saved"
  cmp -s go.sum "${run_root}/go.sum.saved"
  go mod verify
  cmp -s go.mod "${run_root}/go.mod.saved"
  cmp -s go.sum "${run_root}/go.sum.saved"
}

list_package_tests() {
  local package="$1" tags="$2" output line
  local tag_arguments=()
  if [[ -n "${tags}" ]]; then
    tag_arguments=(-tags="${tags}")
  fi
  output="$(go test "${tag_arguments[@]}" -list '^Test' "${package}")" || return 1
  while IFS= read -r line; do
    if [[ "${line}" == Test* ]]; then
      [[ "${line}" =~ ^Test[A-Za-z0-9_]+$ ]] || {
        printf 'invalid top-level test identifier from %s: %q\n' "${package}" "${line}" >&2
        return 1
      }
      printf '%s\n' "${line}"
    fi
  done <<<"${output}"
}

set_difference() {
  local left="$1" right="$2"
  comm -13 <(printf '%s\n' "${left}" | sed '/^$/d' | sort -u) \
    <(printf '%s\n' "${right}" | sed '/^$/d' | sort -u)
}

collect_test_files() {
  local tags="$1" destination_name="$2" output line package tests external_tests
  local -n destination_ref="${destination_name}"
  local arguments=()
  [[ -n "${tags}" ]] && arguments=(-tags="${tags}")
  local template='{{.ImportPath}}|{{join .TestGoFiles ","}}|{{join .XTestGoFiles ","}}'
  output="$(go list "${arguments[@]}" -f "${template}" \
    "${PRODUCT_PACKAGE_PATTERNS[@]}")" || return 1
  while IFS= read -r line; do
    IFS='|' read -r package tests external_tests <<<"${line}"
    [[ -n "${package}" ]] || continue
    destination_ref["${package}"]="${tests}|${external_tests}"
  done <<<"${output}"
}

discover_special_tests() {
  local -A ordinary_files=()
  local -A tagged_files=()
  collect_test_files '' ordinary_files
  collect_test_files 'rootintegration,protocolacceptance' tagged_files
  local package ordinary tagged added
  while IFS= read -r package; do
    [[ -n "${package}" ]] || continue
    [[ "${ordinary_files[${package}]:-}" != "${tagged_files[${package}]}" ]] || continue
    ordinary=''
    if [[ -v "ordinary_files[${package}]" ]]; then
      ordinary="$(list_package_tests "${package}" '')" || return 1
    fi
    tagged="$(list_package_tests "${package}" \
      'rootintegration,protocolacceptance')" || return 1
    added="$(set_difference "${ordinary}" "${tagged}")"
    [[ -n "${added}" ]] || {
      printf 'tagged test files in %s add no top-level tests\n' "${package}" >&2
      return 1
    }
    special_packages+=("${package}")
    ordinary_targets["${package}"]="${ordinary}"
    special_targets["${package}"]="${added}"
  done < <(printf '%s\n' "${!tagged_files[@]}" | sort)
  require_special_inventory
}

require_special_inventory() {
  (( ${#special_packages[@]} != 0 )) || {
    printf 'privileged acceptance package inventory is empty\n' >&2
    return 1
  }
  local package count total=0
  printf 'privileged acceptance mapping:\n'
  for package in "${special_packages[@]}"; do
    count="$(printf '%s\n' "${special_targets[${package}]}" | sed '/^$/d' | wc -l)"
    (( count != 0 )) || {
      printf 'privileged acceptance package %s has no targets\n' "${package}" >&2
      return 1
    }
    total=$((total + count))
    printf '  %s (%d): %s\n' "${package}" "${count}" \
      "$(printf '%s ' ${special_targets[${package}]})"
  done
  printf 'privileged acceptance packages=%d targets=%d\n' \
    "${#special_packages[@]}" "${total}"
}

target_pattern() {
  local targets="$1" joined=''
  while IFS= read -r target; do
    [[ -n "${target}" ]] || continue
    if [[ -n "${joined}" ]]; then
      joined+='|'
    fi
    joined+="${target}"
  done <<<"${targets}"
  printf '^(%s)$\n' "${joined}"
}

write_special_targets() {
  local destination="$1" package target
  : >"${destination}"
  for package in "${special_packages[@]}"; do
    while IFS= read -r target; do
      [[ -n "${target}" ]] && printf '%s\t%s\n' "${package}" "${target}" >>"${destination}"
    done <<<"${special_targets[${package}]}"
  done
}

special_target_names() {
  local package
  for package in "${special_packages[@]}"; do
    printf '%s\n' "${special_targets[${package}]}"
  done | sed '/^$/d' | sort -u
}

require_special_pattern_exclusive() {
  local names="$1" package collisions
  for package in "${special_packages[@]}"; do
    collisions="$(comm -12 \
      <(printf '%s\n' "${ordinary_targets[${package}]}" | sed '/^$/d' | sort -u) \
      <(printf '%s\n' "${names}" | sed '/^$/d' | sort -u))"
    [[ -z "${collisions}" ]] || {
      printf 'privileged target names also select ordinary tests in %s: %s\n' \
        "${package}" "$(printf '%s ' ${collisions})" >&2
      return 1
    }
  done
}

run_special_tests() {
  local expected="${run_root}/privileged.expected"
  local events="${run_root}/privileged.json"
  local names pattern status=0 checker_status=0
  names="$(special_target_names)"
  require_special_pattern_exclusive "${names}"
  pattern="$(target_pattern "${names}")"
  write_special_targets "${expected}"
  set +e
  TRANSFERLANES_FILEVIEWS_ROOTINTEGRATION=1 TRANSFERLANES_TEST_TRANSFERLANES="${run_root}/transferlanes-native" \
    GOMAXPROCS=4 go test -json -count=1 -p 1 -timeout="${ROOT_TEST_TIMEOUT}" \
      -tags=rootintegration,protocolacceptance -run "${pattern}" \
      "${special_packages[@]}" | tee "${events}"
  status=${PIPESTATUS[0]}
  set -e
  "${run_root}/testresult" --expected "${expected}" <"${events}" || checker_status=$?
  if [[ "${status}" -ne 0 || "${checker_status}" -ne 0 ]]; then
    printf 'privileged acceptance failed: go-test=%d result-contract=%d\n' \
      "${status}" "${checker_status}" >&2
    return 1
  fi
}

run_ordinary_checks() {
  go vet ./...
  go test -race ./...
  go build -o "${run_root}/testresult" ./test/default/testresult
}

run_build_checks() {
  ./scripts/test-build.sh
  ./scripts/test-root-build-context.sh --development-commit "${development_commit}" \
    --development-modified "${development_modified}"
  ./scripts/test-development-identity.sh
}

run_root_integration() {
  local native_arch
  native_arch="$(native_linux_arch)"
  local transferlanes_binary="${run_root}/transferlanes-native"
  ./scripts/build.sh --arch "${native_arch}" --output "${transferlanes_binary}" \
    --development-commit "${development_commit}" \
    --development-modified "${development_modified}"
  require_elf_machine "${transferlanes_binary}" "$(elf_machine_for_goarch "${native_arch}")" \
    'native transferlanes'
  printf 'default phase: privileged acceptance\n'
  run_special_tests
  if pgrep -x bindfs >/dev/null; then
    printf 'default test left a bindfs process behind\n' >&2
    return 1
  fi
  if findmnt -rn -t fuse -o TARGET | grep -q .; then
    printf 'default test left a FUSE mount behind\n' >&2
    return 1
  fi
}

native_linux_arch() {
  local host_os
  local host_arch
  host_os="$(go env GOHOSTOS)"
  host_arch="$(go env GOHOSTARCH)"
  if [[ "${host_os}" != 'linux' ]]; then
    printf 'default test Go host OS=%q, want linux\n' "${host_os}" >&2
    return 1
  fi
  case "${host_arch}" in
    amd64)
      [[ "$(uname -m)" == 'x86_64' ]] || {
        printf 'default test Go host and kernel architectures disagree\n' >&2
        return 1
      }
      ;;
    arm64)
      [[ "$(uname -m)" == 'aarch64' ]] || {
        printf 'default test Go host and kernel architectures disagree\n' >&2
        return 1
      }
      ;;
    *)
      printf 'default test Go host architecture=%q is unsupported\n' "${host_arch}" >&2
      return 1
      ;;
  esac
  printf '%s\n' "${host_arch}"
}

elf_machine_for_goarch() {
  case "$1" in
    amd64) printf '%s\n' 'Advanced Micro Devices X86-64' ;;
    arm64) printf '%s\n' 'AArch64' ;;
    *)
      printf 'ELF machine lookup does not support Go architecture %q\n' "$1" >&2
      return 1
      ;;
  esac
}

require_elf_machine() {
  local binary="$1"
  local expected="$2"
  local description="$3"
  local actual
  actual="$(LC_ALL=C readelf -h "${binary}" | sed -n \
    's/^[[:space:]]*Machine:[[:space:]]*//p')"
  if [[ "${actual}" != "${expected}" ]]; then
    printf '%s ELF machine=%q, want %q\n' "${description}" "${actual}" "${expected}" >&2
    return 1
  fi
  printf '%s ELF Machine: %s\n' "${description}" "${actual}"
}

build_and_check_static() {
  local binary="${run_root}/transferlanes-linux-amd64"
  local native_binary="${run_root}/transferlanes-native"
  local dynamic
  local dynamic_nonblank
  local header
  local machine
  local program_headers
  ./scripts/build.sh --arch amd64 --output "${binary}" \
    --development-commit "${development_commit}" \
    --development-modified "${development_modified}"
  header="$(LC_ALL=C readelf -h "${binary}")"
  machine="$(printf '%s\n' "${header}" | sed -n \
    's/^[[:space:]]*Machine:[[:space:]]*//p')"
  if [[ "${machine}" != 'Advanced Micro Devices X86-64' ]]; then
    printf 'static transferlanes machine=%q, want X86-64\n' "${machine}" >&2
    return 1
  fi
  program_headers="$(LC_ALL=C readelf -lW "${binary}")"
  if printf '%s\n' "${program_headers}" | grep -Eq '^[[:space:]]*INTERP[[:space:]]'; then
    printf 'static transferlanes unexpectedly has an ELF interpreter\n' >&2
    return 1
  fi
  dynamic="$(LC_ALL=C readelf -dW "${binary}")"
  dynamic_nonblank="$(printf '%s\n' "${dynamic}" | LC_ALL=C sed '/^[[:space:]]*$/d')"
  if [[ "${dynamic_nonblank}" != 'There is no dynamic section in this file.' ]]; then
    printf 'static transferlanes unexpectedly has a dynamic section\n' >&2
    return 1
  fi
  if [[ "${dynamic}" == *'(NEEDED)'* ]]; then
    printf 'static transferlanes unexpectedly has a needed library\n' >&2
    return 1
  fi
  printf 'ELF Machine: %s\n%s\n' "${machine}" "${dynamic}"
  require_elf_machine "${native_binary}" \
    "$(elf_machine_for_goarch "$(native_linux_arch)")" 'native help binary'
  [[ "$(setpriv --reuid=nobody --regid=nogroup --clear-groups id -u)" == '65534' ]]
  verify_public_cli "${native_binary}"
}

verify_public_cli() {
  local binary="$1" output="${run_root}/cli.stdout" diagnostic="${run_root}/cli.stderr"
  run_public_information() {
    : >"${output}"
    : >"${diagnostic}"
    setpriv --reuid=nobody --regid=nogroup --clear-groups \
      "${binary}" "$@" >"${output}" 2>"${diagnostic}"
    [[ ! -s "${diagnostic}" ]]
  }
  run_public_information --help
  grep -Fx 'Usage:' "${output}"
  run_public_information list --help
  grep -Fx '  transferlanes list [options]' "${output}"
  run_public_information run --help
  grep -Fx '  transferlanes run --source <directory> --network <IPv4>[@weight]...' "${output}"
  run_public_information --version
  printf 'transferlanes devel commit=%s modified=%s\n' \
    "${development_commit}" "${development_modified}" >"${run_root}/version.expected"
  cmp -s "${output}" "${run_root}/version.expected"

  : >"${output}"
  : >"${diagnostic}"
  local status=0
  setpriv --reuid=nobody --regid=nogroup --clear-groups \
    "${binary}" run --source /source --network 192.0.2.1 -- \
    child --help '{}' >"${output}" 2>"${diagnostic}" || status=$?
  [[ "${status}" -eq 1 && ! -s "${output}" ]]
  grep -Fx 'transferlanes: run requires effective root' "${diagnostic}"
  unset -f run_public_information
}

main() {
	if [[ "$#" -ne 0 ]]; then
		printf 'usage: test/default/run.sh\n' >&2
		return 64
	fi
  initialize_run
  printf 'default phase: versions and modules\n'
  print_versions_and_packages
  verify_format_and_modules
  discover_special_tests
  printf 'default phase: vet and ordinary race\n'
  run_ordinary_checks
  printf 'default phase: build entry and secure context\n'
  run_build_checks
  printf 'default phase: root integration\n'
  run_root_integration
  printf 'default phase: static binary\n'
  build_and_check_static
}

main "$@"
