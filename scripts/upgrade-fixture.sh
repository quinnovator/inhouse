#!/usr/bin/env bash
# Write the upgrade fixture for a release: run the seed_test.go beside the
# fixture in a checkout of that release, so the database is exactly what the
# release writes. internal/engine's upgrade test then migrates it.
#
#   scripts/upgrade-fixture.sh v0.1.0
#
# For a new release, copy the previous release's fixture directory, adapt
# seed_test.go to that release's test fakes, and run this.
set -euo pipefail
tag=${1:?release tag required, e.g. v0.1.0}
root=$(git rev-parse --show-toplevel)
fixture=$root/internal/engine/testdata/upgrade/$tag
[[ -f $fixture/seed_test.go ]] || { echo "$fixture/seed_test.go is missing" >&2; exit 1; }
rm -f "$fixture"/inhouse.db* "$fixture/age.key"
work=$(mktemp -d)
trap 'git -C "$root" worktree remove --force "$work"' EXIT
git -C "$root" worktree add --quiet --detach "$work" "refs/tags/$tag"
cp "$fixture/seed_test.go" "$work/internal/engine/upgrade_seed_test.go"
(cd "$work" && INHOUSE_UPGRADE_FIXTURE=$fixture go test -count=1 -run '^TestWriteUpgradeFixture$' ./internal/engine)
# A clean close leaves no WAL behind; the fixture is the database file alone.
[[ ! -e $fixture/inhouse.db-wal ]]
echo "wrote $fixture"
