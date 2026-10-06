#!/usr/bin/env bash
# Nightly (inhouse-backup.timer): snapshot every volume read-only, back up the
# newest snapshots plus tailnet node state with restic, apply retention, check
# the repository, then prune local snapshots and week-old trash.
set -euo pipefail
set -a
source "$CREDENTIALS_DIRECTORY/backup-env"
set +a
export RESTIC_PASSWORD_FILE="$CREDENTIALS_DIRECTORY/restic-password"
export RESTIC_CACHE_DIR=/var/lib/inhouse/backup/cache
root_dir=/var/lib/inhouse
mkdir -p "$root_dir/backup" "$root_dir/volumes/.trash"
exec 9>"$root_dir/backup/lock"
flock -n 9
# Snapshot the actual latest data each night, not just the pre-deploy image.
# Back up only the newest snapshot per volume plus identity state.
manifest="$root_dir/backup/files"
: > "$manifest"
printf '%s\n' "$root_dir/ts" >> "$manifest"
for service_dir in "$root_dir"/volumes/*; do
  [[ -d $service_dir && ! -L $service_dir ]] || continue
  service_name=${service_dir##*/}
  [[ $service_name =~ ^[a-z0-9][a-z0-9-]*$ ]] || continue
  snapshot_dir="$root_dir/snapshots/$service_name"
  mkdir -p "$snapshot_dir"
  for volume_dir in "$service_dir"/*; do
    [[ -d $volume_dir && ! -L $volume_dir ]] || continue
    volume_name=${volume_dir##*/}
    [[ $volume_name =~ ^[a-z0-9][a-z0-9-]*$ ]] || exit 1
    snapshot="$snapshot_dir/$volume_name@backup-$(date -u +%Y%m%dT%H%M%SZ)"
    btrfs subvolume show "$volume_dir" >/dev/null
    btrfs subvolume snapshot -r "$volume_dir" "$snapshot" >/dev/null
    printf '%s\n' "$snapshot" >> "$manifest"
  done
done
restic backup --files-from "$manifest" --tag inhouse-nightly
# Snapshot paths change nightly. Group by stable host/tag identity so retention
# applies across backup runs instead of retaining every unique path forever.
restic forget --tag inhouse-nightly --group-by host,tags --keep-daily 7 --keep-weekly 4 --keep-monthly 6 --prune
restic check
# Local RO backup snapshots retain the newest five per service/volume.
bash /usr/local/libexec/inhouse/retain.sh "$root_dir"
