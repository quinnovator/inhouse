#!/usr/bin/env bash
# Root on a freshly provisioned destination, with the original daemon stopped.
# Arguments: verified Litestream DB, restic restore root, out-of-band age key.
# Refuses to overwrite existing state, keys, identities or volume data. The
# empty ts/ and volumes/ directories created by provisioning are accepted.
set -euo pipefail
[[ $EUID -eq 0 ]]
database=${1:?Restored SQLite database required}
restore_root=${2:?Restic restore directory required}
age_key=${3:?Out-of-band age identity required}
root_dir=/var/lib/inhouse
[[ -f $database && -f $age_key ]]
command -v sqlite3 >/dev/null || { echo 'sqlite3 is required' >&2; exit 1; }
subgid_base=$(awk -F: '$1 == "inhouse" { print $2; exit }' /etc/subgid)
[[ $subgid_base =~ ^[0-9]+$ ]]
[[ $(findmnt -no FSTYPE --target "$root_dir") == btrfs ]]
! systemctl is-active --quiet inhoused.service
empty_or_absent() { [[ ! -e $1 ]] || { [[ -d $1 && ! -L $1 && -z $(ls -A "$1") ]]; }; }
[[ ! -e $root_dir/state/inhouse.db && ! -e $root_dir/keys/age.key ]]
empty_or_absent "$root_dir/ts"
empty_or_absent "$root_dir/volumes"
[[ -d $restore_root/var/lib/inhouse/ts ]]
install -o inhouse -g inhouse -m 0700 -d "$root_dir/state" "$root_dir/keys" "$root_dir/volumes" "$root_dir/snapshots" "$root_dir/home"
install -o inhouse -g inhouse -m 0600 "$database" "$root_dir/state/inhouse.db"
install -o inhouse -g inhouse -m 0600 "$age_key" "$root_dir/keys/age.key"
# cp -a would nest the copy inside an existing directory.
[[ ! -e $root_dir/ts ]] || rmdir "$root_dir/ts"
cp -a "$restore_root/var/lib/inhouse/ts" "$root_dir/ts"
# Do not recursively chown restored data: subordinate ownership and ACLs matter.
for service_dir in "$restore_root"/var/lib/inhouse/snapshots/*; do
  [[ -d $service_dir && ! -L $service_dir ]] || continue
  service_name=${service_dir##*/}
  [[ $service_name =~ ^[a-z0-9][a-z0-9-]*$ ]] || exit 1
  # Restore only live services' data, and only data whose ACL grants the
  # group of the ID slot the database gives the service now: the nightly
  # backup can predate the database, and a deleted ephemeral service's slot
  # may since belong to another service.
  slot=$(sqlite3 -readonly "$database" "SELECT n.slot FROM namespaces n JOIN services s ON s.name = n.service WHERE n.service = '$service_name' AND s.deleted_at IS NULL")
  if [[ ! $slot =~ ^[0-9]+$ ]]; then
    echo "skipping $service_name: not a live service in the restored database" >&2
    continue
  fi
  gid=$((subgid_base + slot * 65536))
  install -o inhouse -g inhouse -m 0700 -d "$root_dir/volumes/$service_name"
  for snapshot in "$service_dir"/*; do
    [[ -d $snapshot && ! -L $snapshot ]] || continue
    volume_name=${snapshot##*/}; volume_name=${volume_name%@*}
    [[ $volume_name =~ ^[a-z0-9][a-z0-9-]*$ ]] || exit 1
    if ! getfacl -cpnE "$snapshot" | grep -qx "group:$gid:rwx"; then
      echo "skipping $service_name/$volume_name: its data belongs to a different ID slot" >&2
      continue
    fi
    destination="$root_dir/volumes/$service_name/$volume_name"
    [[ ! -e $destination ]]
    btrfs subvolume create "$destination" >/dev/null
    cp -a "$snapshot/." "$destination/"
    chown --reference="$snapshot" "$destination"
    chmod --reference="$snapshot" "$destination"
    getfacl -cp "$snapshot" | setfacl --set-file=- "$destination"
  done
done
restorecon -RF "$root_dir/state" "$root_dir/keys" "$root_dir/ts" "$root_dir/volumes"
echo 'Restored database, identities, age key and btrfs volume data; daemon remains stopped.'
