#!/usr/bin/env bash
# Keep the newest five nightly snapshots per volume; empty trash older than
# seven days. Called by backup.sh.
set -euo pipefail
root_dir=${1:?data root required}
for service_dir in "$root_dir"/snapshots/*; do
  [[ -d $service_dir && ! -L $service_dir ]] || continue
  declare -A volume_names=()
  for snapshot in "$service_dir"/*@backup-*; do
    [[ -d $snapshot && ! -L $snapshot ]] || continue
    volume_name=${snapshot##*/};volume_name=${volume_name%@backup-*}
    [[ $volume_name =~ ^[a-z0-9-]+$ ]] || exit 1
    volume_names[$volume_name]=1
  done
  for volume_name in "${!volume_names[@]}"; do
    mapfile -t snapshots < <(printf '%s\n' "$service_dir/$volume_name"@backup-* | sort -r)
    for snapshot in "${snapshots[@]:5}"; do
      [[ -d $snapshot && ! -L $snapshot ]] || exit 1
      btrfs subvolume delete "$snapshot" >/dev/null
    done
  done
  unset volume_names
done
for service_dir in "$root_dir"/volumes/.trash/*; do
  [[ -d $service_dir && ! -L $service_dir ]] || continue
  deleted=${service_dir##*@}
  [[ $deleted =~ ^[0-9]+$ ]] || exit 1
  (( $(date +%s) - deleted >= 604800 )) || continue
  if btrfs subvolume show "$service_dir" >/dev/null 2>&1; then
    # An explicit volume rollback retains the entire previous subvolume here.
    btrfs subvolume delete "$service_dir" >/dev/null
    continue
  fi
  for volume_dir in "$service_dir"/*; do
    [[ -d $volume_dir && ! -L $volume_dir ]] || exit 1
    btrfs subvolume delete "$volume_dir" >/dev/null
  done
  rmdir "$service_dir"
done
