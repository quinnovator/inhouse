#!/usr/bin/env bash
# Install or upgrade inhoused. Run as root on the host from a directory
# containing the inhoused binary, inhoused.sha512 and inhoused.service.
# Usage: install.sh CAPABILITY ENROLL_CLIENT_ID LIFECYCLE_CLIENT_ID
#
# Copy the OAuth secrets first, as files and never as arguments:
#   /etc/inhouse/enroll-secret      (root, 0600)
#   /etc/inhouse/lifecycle-secret   (root, 0600)
set -euo pipefail
[[ $EUID -eq 0 ]] || { echo 'run as root' >&2; exit 1; }
stage=$(pwd)
capability=${1:?capability name required, e.g. example.com/cap/inhouse}
enroll_id=${2:?enroll OAuth client ID required}
lifecycle_id=${3:?lifecycle OAuth client ID required}
[[ $capability =~ ^[a-zA-Z0-9./_-]+$ && $enroll_id =~ ^[a-zA-Z0-9]+$ && $lifecycle_id =~ ^[a-zA-Z0-9]+$ ]]
for f in /etc/inhouse/enroll-secret /etc/inhouse/lifecycle-secret; do
  [[ -f $f ]] || { echo "missing $f" >&2; exit 1; }
  chmod 0600 "$f"
done
sha512sum -c inhoused.sha512
cd /
runuser -u inhouse -- env HOME=/var/lib/inhouse/home XDG_RUNTIME_DIR=/run/user/1500 systemctl --user enable --now podman.socket
install -o inhouse -g inhouse -m 0700 -d /var/lib/inhouse/state /var/lib/inhouse/ts
install -o root -g root -m 0755 "$stage/inhoused" /usr/local/bin/.inhoused.new
mv -Tf /usr/local/bin/.inhoused.new /usr/local/bin/inhoused
install -o root -g root -m 0644 "$stage/inhoused.service" /etc/systemd/system/inhoused.service
install -o root -g root -m 0600 /dev/null /etc/inhouse/inhoused.env
cat > /etc/inhouse/inhoused.env <<CONF
CAPABILITY=$capability
ENROLL_CLIENT_ID=$enroll_id
LIFECYCLE_CLIENT_ID=$lifecycle_id
CONF
restorecon -F /usr/local/bin/inhoused /etc/systemd/system/inhoused.service /etc/inhouse/inhoused.env
systemctl daemon-reload
systemctl enable inhoused.service
systemctl restart inhoused.service
echo 'inhoused restarted; follow it with: journalctl -fu inhoused'
