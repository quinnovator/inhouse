// Package volumes keeps service data in btrfs subvolumes.
//
//	volumes/<service>/<volume>          live data, one subvolume each
//	snapshots/<service>/<volume>@r<N>   read-only, taken before revision N starts
//	volumes/.trash/                     deleted or replaced data, kept seven days
//
// Volume roots stay owned by the inhouse user. A POSIX ACL grants the
// service's mapped group access, so ownership never changes between
// revisions and every snapshot stays readable for backup.
package volumes

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/quinnovator/inhouse/internal/spec"
	"github.com/quinnovator/inhouse/internal/store"
	"github.com/quinnovator/inhouse/internal/userns"
)

// KeepSnapshots is how many pre-deploy snapshots each volume retains.
const KeepSnapshots = 5

type Manager struct {
	Root string // the data root, e.g. /var/lib/inhouse
	IDs  userns.Map
}

func run(ctx context.Context, name string, args ...string) error {
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %s failed: %s", name, args[0], strings.TrimSpace(firstLine(string(out))))
	}
	return nil
}

func firstLine(s string) string {
	s, _, _ = strings.Cut(s, "\n")
	return s
}

// directory refuses anything but a real directory, so a planted symlink can
// never redirect a privileged-looking operation.
func directory(p string) error {
	info, err := os.Lstat(p)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%s must be a directory", p)
	}
	return nil
}

func (m *Manager) snapshotDir(service string) string {
	return filepath.Join(m.Root, "snapshots", service)
}

func snapshotName(volume string, rev int) string { return volume + "@r" + strconv.Itoa(rev) }

// Ensure creates the volume's subvolume if needed, grants the service's
// mapped group access, and returns its path.
func (m *Manager) Ensure(ctx context.Context, service, volume string) (string, error) {
	if !spec.ValidName(service) || !spec.ValidName(volume) {
		return "", errors.New("invalid volume identity")
	}
	offset, err := m.IDs.Offset(ctx, service)
	if err != nil {
		return "", err
	}
	base := filepath.Join(m.Root, "volumes")
	parent := filepath.Join(base, service)
	if err = directory(base); err != nil {
		return "", err
	}
	if err = os.Mkdir(parent, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return "", err
	}
	if err = directory(parent); err != nil {
		return "", err
	}
	dst := filepath.Join(parent, volume)
	if _, err = os.Lstat(dst); errors.Is(err, os.ErrNotExist) {
		if err = run(ctx, "btrfs", "subvolume", "create", dst); err != nil {
			return "", err
		}
	} else if err != nil {
		return "", err
	}
	if err = directory(dst); err != nil {
		return "", err
	}
	gid := m.IDs.HostID(offset)
	acl := fmt.Sprintf("g:%d:rwx,d:g:%d:rwx,d:u:%d:rwx", gid, gid, os.Getuid())
	if err = run(ctx, "setfacl", "-m", acl, dst); err != nil {
		return "", err
	}
	return dst, nil
}

// Snapshot takes a read-only snapshot of each of r's volumes before r starts,
// then prunes each volume to its newest KeepSnapshots.
func (m *Manager) Snapshot(ctx context.Context, r store.Revision) error {
	for _, volume := range r.Spec.VolumeNames() {
		src, err := m.Ensure(ctx, r.Service, volume)
		if err != nil {
			return err
		}
		parent := m.snapshotDir(r.Service)
		if err = os.MkdirAll(parent, 0o700); err != nil {
			return err
		}
		if err = directory(parent); err != nil {
			return err
		}
		dst := filepath.Join(parent, snapshotName(volume, r.Rev))
		if _, err = os.Lstat(dst); errors.Is(err, os.ErrNotExist) {
			if err = run(ctx, "btrfs", "subvolume", "snapshot", "-r", src, dst); err != nil {
				return err
			}
		} else if err != nil {
			return err
		}
		if err = m.prune(ctx, parent, volume); err != nil {
			return err
		}
	}
	return nil
}

func (m *Manager) prune(ctx context.Context, parent, volume string) error {
	entries, err := os.ReadDir(parent)
	if err != nil {
		return err
	}
	revs := []int{}
	for _, f := range entries {
		if rest, ok := strings.CutPrefix(f.Name(), volume+"@r"); ok {
			if rev, err := strconv.Atoi(rest); err == nil {
				revs = append(revs, rev)
			}
		}
	}
	sort.Sort(sort.Reverse(sort.IntSlice(revs)))
	for _, rev := range revs[min(len(revs), KeepSnapshots):] {
		if err = removeSubvolume(ctx, filepath.Join(parent, snapshotName(volume, rev))); err != nil {
			return err
		}
	}
	return nil
}

// removeSubvolume deletes a subvolume as its unprivileged owner. Deleting a
// read-only snapshot fails with EROFS, so only a snapshot being discarded is
// made writable first (the filesystem is mounted user_subvol_rm_allowed).
func removeSubvolume(ctx context.Context, p string) error {
	if err := directory(p); err != nil {
		return err
	}
	if err := run(ctx, "btrfs", "property", "set", "-ts", p, "ro", "false"); err != nil {
		return err
	}
	return run(ctx, "btrfs", "subvolume", "delete", p)
}

// Delete removes a deleted service's data. A persistent service's volumes move
// to trash, where the nightly backup job removes them after seven days.
// An ephemeral service's volumes and snapshots go immediately.
func (m *Manager) Delete(ctx context.Context, svc store.Service) error {
	parent := filepath.Join(m.Root, "volumes", svc.Name)
	if svc.Kind == store.Persistent {
		if _, err := os.Lstat(parent); errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err := directory(parent); err != nil {
			return err
		}
		trash := filepath.Join(m.Root, "volumes", ".trash")
		if err := os.MkdirAll(trash, 0o700); err != nil {
			return err
		}
		return os.Rename(parent, filepath.Join(trash, svc.Name+"@"+strconv.FormatInt(svc.Deleted, 10)))
	}
	for _, dir := range []string{parent, m.snapshotDir(svc.Name)} {
		entries, err := os.ReadDir(dir)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		if err = directory(dir); err != nil {
			return err
		}
		for _, f := range entries {
			if err = removeSubvolume(ctx, filepath.Join(dir, f.Name())); err != nil {
				return err
			}
		}
		if err = os.Remove(dir); err != nil {
			return err
		}
	}
	return nil
}
