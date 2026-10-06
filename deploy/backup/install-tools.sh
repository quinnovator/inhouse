#!/usr/bin/env bash
# Install checksum-verified Litestream and restic (amd64) to /usr/local/bin.
set -euo pipefail
[[ $EUID -eq 0 ]]
work_dir=$(mktemp -d)
trap 'rm -rf "$work_dir"' EXIT
cd "$work_dir"
curl -fL --silent --show-error https://github.com/benbjohnson/litestream/releases/download/v0.5.17/litestream-0.5.17-linux-x86_64.tar.gz -o litestream.tar.gz
printf '%s  %s\n' cfb371176d164437ae869f8351cfde49bd1804ae71c61923f75c9cba9c9c006d litestream.tar.gz | sha256sum -c
curl -fL --silent --show-error https://github.com/restic/restic/releases/download/v0.19.1/restic_0.19.1_linux_amd64.bz2 -o restic.bz2
printf '%s  %s\n' f415415624dcc452f2a02b8c33641791a8c6d6d3b65bbb3543fcf9a25151585c restic.bz2 | sha256sum -c
tar -xzf litestream.tar.gz
bzip2 -dc restic.bz2 > restic
install -o root -g root -m 0755 litestream restic /usr/local/bin/
restorecon -F /usr/local/bin/litestream /usr/local/bin/restic
