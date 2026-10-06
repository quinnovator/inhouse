#!/usr/bin/env bash
# Install the backup units. Run as root from this directory.
# Requires /etc/inhouse/backup.env, restic-password and litestream.yml.
set -euo pipefail
[[ $EUID -eq 0 ]]
[[ -f /etc/inhouse/backup.env && -f /etc/inhouse/restic-password && -f /etc/inhouse/litestream.yml ]]
install -m 0755 -d /usr/local/libexec/inhouse
install -o root -g root -m 0755 litestream-start.sh backup.sh retain.sh /usr/local/libexec/inhouse/
install -o root -g root -m 0644 inhouse-litestream.service inhouse-backup.service inhouse-backup.timer /etc/systemd/system/
install -o root -g root -m 0700 -d /var/lib/inhouse/backup /var/lib/inhouse/volumes/.trash
chmod 0600 /etc/inhouse/backup.env /etc/inhouse/restic-password
# Config contains no credentials; systemd exposes credentials to the process.
chmod 0644 /etc/inhouse/litestream.yml
restorecon -RF /usr/local/libexec/inhouse /etc/systemd/system/inhouse-litestream.service /etc/systemd/system/inhouse-backup.service /etc/systemd/system/inhouse-backup.timer
systemctl daemon-reload
systemctl start inhouse-backup.service
systemctl enable --now inhouse-litestream.service inhouse-backup.timer
