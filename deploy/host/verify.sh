#!/usr/bin/env bash
# Run on the provisioned host as core: sudo bash verify.sh
# Add --locked for final acceptance after public SSH removal.
set -euo pipefail
[[ $EUID -eq 0 ]]
[[ $# -eq 0 || ( $# -eq 1 && $1 == --locked ) ]] || { echo 'Usage: verify.sh [--locked]' >&2; exit 1; }
cd /
source /etc/os-release
[[ $ID == fedora && $VARIANT_ID == coreos ]]
printf 'OS: %s\n' "$PRETTY_NAME"
findmnt --target /var/lib/inhouse --output TARGET,SOURCE,FSTYPE,OPTIONS
[[ $(findmnt -n -o FSTYPE --target /var/lib/inhouse) == btrfs ]]
[[ $(id -u inhouse) == 1500 ]]
[[ -f /var/lib/systemd/linger/inhouse ]]
systemctl is-active user@1500.service inhouse-firewall.service tailscaled.service
systemctl --no-pager --full status inhouse-tailscale-install.service || true
rpm-ostree status
runuser -u inhouse -- env HOME=/var/lib/inhouse/home XDG_RUNTIME_DIR=/run/user/1500 \
  podman info --format '{{.Host.Security.Rootless}} {{.Store.GraphRoot}}'
rootless=$(runuser -u inhouse -- env HOME=/var/lib/inhouse/home XDG_RUNTIME_DIR=/run/user/1500 \
  podman info --format '{{.Host.Security.Rootless}}')
[[ $rootless == true ]]
[[ -S /run/user/1500/podman/podman.sock ]]
curl --fail --silent --unix-socket /run/user/1500/podman/podman.sock http://localhost/_ping
echo
tailscale status --peers=false
tailscale ip -4
nft list table inet inhouse
if [[ ${1:-} == --locked ]]; then
  [[ $(readlink /etc/inhouse/firewall-active.nft) == /etc/inhouse/firewall-locked.nft ]]
  ! nft list table inet inhouse | grep -q 'tcp dport 22'
  echo 'Persistent locked firewall selected; bootstrap public SSH rule absent.'
fi
echo 'External TCP scan and two independent tailnet SSH sessions must be verified from the operator machine.'
