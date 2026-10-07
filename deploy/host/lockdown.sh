#!/usr/bin/env bash
# Remove public SSH. Run as root through tailnet SSH, only after two separate
# tailnet SSH sessions have worked. Usage: lockdown.sh --approved
set -euo pipefail
[[ $EUID -eq 0 ]]
cd /
[[ ${1:-} == --approved ]] || { echo 'Require explicit operator approval: --approved' >&2; exit 1; }
systemctl is-active --quiet tailscaled.service
tailscale ip -4 >/dev/null
rules=/etc/inhouse/firewall-locked.nft
active=/etc/inhouse/firewall-active.nft
[[ $(head -n 1 "$rules") == 'table inet inhouse {}' ]]
if grep -q 'tcp dport 22' "$rules"; then
  echo "$rules still allows tcp dport 22; refusing to lock down" >&2
  exit 1
fi
nft -c -f "$rules"
previous=$(readlink "$active")
# Stage the persistent selector first. Restore it if the atomic nft load fails.
ln -s "$rules" "$active.next"
mv -Tf "$active.next" "$active"
if ! nft -f "$active"; then
  ln -s "$previous" "$active.next"
  mv -Tf "$active.next" "$active"
  exit 1
fi
nft list table inet inhouse
echo 'Public SSH removed. Verify a fresh tailnet SSH session and an external TCP scan.'
