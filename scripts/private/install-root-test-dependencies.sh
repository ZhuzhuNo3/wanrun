#!/usr/bin/env bash
set -euo pipefail

readonly SNAPSHOT_MAIN='https://snapshot.debian.org/archive/debian/20251118T203001Z'
readonly SNAPSHOT_SECURITY='https://snapshot.debian.org/archive/debian-security/20251118T225330Z'
readonly PACKAGES=(
  'rsync=3.2.7-1+deb12u2'
  'openssh-client=1:9.2p1-2+deb12u7'
  'openssh-server=1:9.2p1-2+deb12u7'
  'iproute2=6.1.0-3'
  'iptables=1.8.9-2'
  'nftables=1.0.6-2+deb12u2'
  'util-linux=2.38.1-5+deb12u3'
  'procps=2:4.0.2-3'
  'bindfs=1.14.7-1'
  'fuse=2.9.9-6+b1'
  'ca-certificates=20230311+deb12u1'
)

configure_apt_snapshot() {
  find /etc/apt/sources.list.d -mindepth 1 -maxdepth 1 -type f -delete
  rm -f /etc/apt/sources.list
  printf '%s\n' \
    "deb [check-valid-until=no] ${SNAPSHOT_MAIN} bookworm main" \
    "deb [check-valid-until=no] ${SNAPSHOT_SECURITY} bookworm-security main" \
    >/etc/apt/sources.list
}

install_pinned_packages() {
  apt-get update
  DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends "${PACKAGES[@]}"
  for specification in "${PACKAGES[@]}"; do
    local package="${specification%%=*}"
    local expected="${specification#*=}"
    local actual
    actual="$(dpkg-query -W -f='${Version}' "${package}")"
    [[ "${actual}" == "${expected}" ]]
  done
}

verify_executables() {
  local executable
  for executable in go aws minio rsync ssh sshd ssh-keygen ip iptables iptables-save \
    ip6tables-save nft unshare nsenter setpriv ps sysctl readelf flock bindfs fusermount; do
    command -v "${executable}" >/dev/null
  done
  ! command -v docker >/dev/null 2>&1
}

prepare_runtime_directories() {
  if ! id test >/dev/null 2>&1; then
    useradd --create-home --uid 10001 --user-group --shell /bin/bash test
    passwd -d test
  fi
  [[ "$(id -u test)" == '10001' ]]
  install -d -o root -g root -m 0755 /run/sshd /srv/rsync
}

main() {
  [[ "$#" -eq 0 ]]
  configure_apt_snapshot
  install_pinned_packages
  verify_executables
  prepare_runtime_directories
  rm -rf /var/lib/apt/lists/*
}

main "$@"
