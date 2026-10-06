package volumes

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/quinnovator/inhouse/internal/spec"
	"github.com/quinnovator/inhouse/internal/store"
)

// CheckRestore verifies that each of r's volumes has a read-only snapshot
// named for r.
func (m *Manager) CheckRestore(ctx context.Context, r store.Revision) error {
	if !spec.ValidName(r.Service) || r.Rev < 1 {
		return errors.New("invalid restore target")
	}
	volumes := r.Spec.VolumeNames()
	if len(volumes) == 0 {
		return fmt.Errorf("r%d has no volumes to restore", r.Rev)
	}
	for _, volume := range volumes {
		p := filepath.Join(m.snapshotDir(r.Service), snapshotName(volume, r.Rev))
		if directory(p) != nil {
			return fmt.Errorf("no snapshot of %s from r%d (only the newest %d per volume are kept)", volume, r.Rev, KeepSnapshots)
		}
		// Unprivileged `btrfs subvolume show` fails; the property read works.
		out, err := exec.CommandContext(ctx, "btrfs", "property", "get", "-ts", p, "ro").Output()
		if err != nil || strings.TrimSpace(string(out)) != "ro=true" {
			return fmt.Errorf("snapshot of %s from r%d is not read-only", volume, r.Rev)
		}
	}
	return nil
}

// Restore replaces each of candidate's volumes with a writable copy of its
// snapshot from revision from. The previous data moves to trash. The writer
// must be stopped first. Every step is resumable: a restart mid-restore
// continues from the files already in place, and a synced marker records
// each finished volume before the candidate can write to it.
func (m *Manager) Restore(ctx context.Context, candidate store.Revision, from int) error {
	if !spec.ValidName(candidate.Service) || candidate.Rev < 1 || from < 1 {
		return errors.New("invalid volume restore")
	}
	parent := filepath.Join(m.Root, "volumes", candidate.Service)
	snapshots := m.snapshotDir(candidate.Service)
	trash := filepath.Join(m.Root, "volumes", ".trash")
	if err := os.MkdirAll(trash, 0o700); err != nil {
		return err
	}
	for _, volume := range candidate.Spec.VolumeNames() {
		marker := filepath.Join(snapshots, fmt.Sprintf(".restored-%s@r%d", volume, candidate.Rev))
		if info, err := os.Lstat(marker); err == nil {
			if !info.Mode().IsRegular() {
				return errors.New("invalid restore marker")
			}
			continue
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		source := filepath.Join(snapshots, snapshotName(volume, from))
		live := filepath.Join(parent, volume)
		staged := filepath.Join(parent, fmt.Sprintf(".restore-r%d-%s", candidate.Rev, volume))
		replaced := filepath.Join(trash, fmt.Sprintf("%s-%s-r%d@%d", candidate.Service, volume, candidate.Rev, candidate.Created))

		// 1. Stage a writable copy of the snapshot, then move live data aside.
		if _, err := os.Lstat(replaced); errors.Is(err, os.ErrNotExist) {
			if err = directory(live); err != nil {
				return err
			}
			if _, err = os.Lstat(staged); errors.Is(err, os.ErrNotExist) {
				if err = directory(source); err != nil {
					return err
				}
				if err = run(ctx, "btrfs", "subvolume", "snapshot", source, staged); err != nil {
					return err
				}
			} else if err != nil {
				return err
			}
			if err = directory(staged); err != nil {
				return err
			}
			if err = os.Rename(live, replaced); err != nil {
				return err
			}
		} else if err != nil {
			return err
		}
		// 2. Move the staged copy into place.
		if _, err := os.Lstat(staged); err == nil {
			if _, err = os.Lstat(live); !errors.Is(err, os.ErrNotExist) {
				return errors.New("restore destination unexpectedly exists")
			}
			if err = os.Rename(staged, live); err != nil {
				return err
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if err := directory(live); err != nil {
			return err
		}
		// 3. Persist the renames, then the marker.
		if err := run(ctx, "sync", "-f", parent); err != nil {
			return err
		}
		f, err := os.OpenFile(marker, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return err
		}
		err = f.Sync()
		_ = f.Close()
		if err != nil {
			return err
		}
		if err = run(ctx, "sync", "-f", snapshots); err != nil {
			return err
		}
	}
	return nil
}
