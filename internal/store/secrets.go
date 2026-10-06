package store

import (
	"context"
	"database/sql"
	"encoding/json"

	"github.com/quinnovator/inhouse/internal/spec"
)

// SecretInfo is everything about a secret that may leave the platform.
type SecretInfo struct {
	Name      string `json:"name"`
	UpdatedBy string `json:"updated_by"`
	UpdatedAt int64  `json:"updated_at"`
}

func (s *Store) PutSecret(ctx context.Context, name string, ciphertext []byte, actor string) error {
	return s.write(ctx, func(tx *sql.Tx) error {
		if _, err := tx.Exec("INSERT INTO secrets(name,ciphertext,updated_by,updated_at) VALUES(?,?,?,?) ON CONFLICT(name) DO UPDATE SET ciphertext=excluded.ciphertext,updated_by=excluded.updated_by,updated_at=excluded.updated_at",
			name, ciphertext, actor, now()); err != nil {
			return err
		}
		return event(tx, "", 0, actor, "secret_set", "secret "+name+" set")
	})
}

func (s *Store) Secret(ctx context.Context, name string) ([]byte, error) {
	var c []byte
	err := s.db.QueryRowContext(ctx, "SELECT ciphertext FROM secrets WHERE name=?", name).Scan(&c)
	return c, notFound(err)
}

func (s *Store) Secrets(ctx context.Context) ([]SecretInfo, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT name,updated_by,updated_at FROM secrets ORDER BY name")
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []SecretInfo{}
	for rows.Next() {
		var v SecretInfo
		if err := rows.Scan(&v.Name, &v.UpdatedBy, &v.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// DeleteSecret refuses while any revision that still has a pod references it.
func (s *Store) DeleteSecret(ctx context.Context, name, actor string) error {
	return s.write(ctx, func(tx *sql.Tx) error {
		rows, err := tx.Query("SELECT service,rev,spec_json FROM revisions WHERE host_port IS NOT NULL")
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var service, raw string
			var rev int
			if err := rows.Scan(&service, &rev, &raw); err != nil {
				return err
			}
			var stack spec.Stack
			if err := json.Unmarshal([]byte(raw), &stack); err != nil {
				return err
			}
			for _, c := range stack.Containers {
				for _, ref := range c.Secrets {
					if ref == name {
						return &Conflict{Message: "secret is used by " + service + " (a revision that still has a pod)"}
					}
				}
			}
		}
		if err := rows.Err(); err != nil {
			return err
		}
		res, err := tx.Exec("DELETE FROM secrets WHERE name=?", name)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotFound
		}
		return event(tx, "", 0, actor, "secret_deleted", "secret "+name+" deleted")
	})
}

// PinSecretVersion keeps a ciphertext forever under its hash, so a revision
// can always be restarted with the exact value it was deployed with.
func (s *Store) PinSecretVersion(ctx context.Context, hash string, ciphertext []byte) error {
	return s.write(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec("INSERT INTO secret_versions(hash,ciphertext) VALUES(?,?) ON CONFLICT(hash) DO NOTHING", hash, ciphertext)
		return err
	})
}

func (s *Store) SecretVersion(ctx context.Context, hash string) ([]byte, error) {
	var c []byte
	err := s.db.QueryRowContext(ctx, "SELECT ciphertext FROM secret_versions WHERE hash=?", hash).Scan(&c)
	return c, notFound(err)
}

// AllSecretCiphertexts returns every current and pinned value, for redaction.
func (s *Store) AllSecretCiphertexts(ctx context.Context) ([][]byte, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT ciphertext FROM secret_versions UNION SELECT ciphertext FROM secrets")
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := [][]byte{}
	for rows.Next() {
		var c []byte
		if err := rows.Scan(&c); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}
