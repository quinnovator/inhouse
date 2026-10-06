#!/usr/bin/env bash
# Continuously replicate the SQLite database (inhouse-litestream.service).
set -euo pipefail
# Operator-owned environment file, never passed as command arguments.
set -a
source "$CREDENTIALS_DIRECTORY/backup-env"
set +a
exec /usr/local/bin/litestream replicate -config "$CREDENTIALS_DIRECTORY/litestream-config"
