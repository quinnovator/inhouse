package volumes

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestTrashOwnedBySomeoneElseSaysSo: a trash folder the daemon can't write
// (root created it) fails with the fix in the message, not just "permission
// denied".
func TestTrashOwnedBySomeoneElseSaysSo(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can write any folder")
	}
	root := t.TempDir()
	live, trash := filepath.Join(root, "blog"), filepath.Join(root, ".trash")
	if err := os.Mkdir(live, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(trash, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(trash, 0o700) })
	err := toTrash(live, filepath.Join(trash, "blog@1"))
	if err == nil || !strings.Contains(err.Error(), trash+" must be owned by the inhouse user") {
		t.Fatalf("error %v", err)
	}
	if err = os.Chmod(trash, 0o700); err != nil {
		t.Fatal(err)
	}
	if err = toTrash(live, filepath.Join(trash, "blog@1")); err != nil {
		t.Fatal(err)
	}
}
