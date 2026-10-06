#!/usr/bin/env bash
# Render host.bu.tmpl with an instance's values and compile it to Ignition.
# Usage: render.sh INSTANCE_DIR
#   INSTANCE_DIR/instance.env must define HOSTNAME, SSH_PUBKEY and DATA_DISK
#   (the data disk by hardware ID, e.g. /dev/disk/by-id/nvme-...).
# Writes INSTANCE_DIR/host.bu and INSTANCE_DIR/host.ign. Neither contains
# secrets; credentials are copied to the host separately.
set -euo pipefail
here=$(cd "$(dirname "$0")" && pwd)
instance_dir=${1:?usage: render.sh INSTANCE_DIR}
set -a
source "$instance_dir/instance.env"
set +a
python3 - "$here/host.bu.tmpl" "$instance_dir/host.bu" <<'PY'
import json, os, re, sys
template = open(sys.argv[1]).read()
for name in set(re.findall(r'\$\{([A-Z_]+)\}', template)):
    value = os.environ.get(name, '')
    if not value or '\n' in value or '\r' in value:
        raise SystemExit(f'missing or invalid {name}')
    template = template.replace('${' + name + '}', json.dumps(value))
if not re.fullmatch(r'[a-z0-9][a-z0-9-]{0,62}', os.environ['HOSTNAME']):
    raise SystemExit('HOSTNAME must be a DNS label')
if not re.fullmatch(r'/dev/disk/by-id/[a-zA-Z0-9_.:-]+', os.environ['DATA_DISK']):
    raise SystemExit('DATA_DISK must name the disk by hardware ID (/dev/disk/by-id/...)')
open(sys.argv[2], 'w').write(template)
PY
engine=$(command -v podman || command -v docker)
"$engine" run --rm -i \
  quay.io/coreos/butane@sha256:d264fba5a02ec7a5525b7cd4ab04090e8c70d0ee42a74a90a0e2f89633ae720c \
  --pretty --strict < "$instance_dir/host.bu" > "$instance_dir/host.ign.tmp"
mv "$instance_dir/host.ign.tmp" "$instance_dir/host.ign"
echo "validated Ignition: $instance_dir/host.ign"
