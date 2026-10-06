// Package userns gives every service its own fixed slice of subordinate IDs.
//
// The inhouse user owns a range of subordinate UIDs and GIDs (/etc/subuid).
// Each service gets a durable 65536-ID slot in it, shared by all of its
// revisions, so container root never maps to the inhouse user (which owns
// the database, keys and node state) and volume ownership stays stable
// across deploys. Slots are never reused.
package userns

import (
	"context"

	"github.com/quinnovator/inhouse/internal/store"
)

// Size is the number of IDs in one service's namespace.
const Size = 65536

type Map struct {
	Store *store.Store
	// Base and Count describe the inhouse user's subordinate range.
	Base, Count int
}

// Slots is how many services the range can ever hold.
func (m Map) Slots() int { return m.Count / Size }

// Offset is where the service's container ID 0 lands inside the rootless
// user namespace (where 0 is the inhouse user and 1..Count its subordinates).
func (m Map) Offset(ctx context.Context, service string) (int, error) {
	slot, err := m.Store.Namespace(ctx, service, m.Slots())
	return 1 + slot*Size, err
}

// HostID is the real host ID for container ID 0 at offset.
func (m Map) HostID(offset int) int { return m.Base + offset - 1 }
