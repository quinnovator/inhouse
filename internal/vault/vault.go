// Package vault encrypts secret values with age. Values are decrypted only to
// hand them to Podman or to redact them from logs; nothing here returns a
// value to a caller of the API.
package vault

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"filippo.io/age"
	"github.com/quinnovator/inhouse/internal/spec"
	"github.com/quinnovator/inhouse/internal/store"
)

// MaxValue bounds a secret value.
const MaxValue = 32 << 10

// RegistryPrefix names platform-only secrets holding registry credentials,
// e.g. registry-auth/ghcr.io. They are used for image pulls and cannot be
// referenced by a stack.
const RegistryPrefix = "registry-auth/"

type Vault struct {
	store    *store.Store
	identity *age.X25519Identity

	mu sync.Mutex
	// redactor matches every value in plain, which holds the decrypted
	// values Redact last saw, keyed by ciphertext hash.
	redactor *matcher
	plain    map[[32]byte][]byte
}

// Open loads the age identity at keyFile, generating it on first start.
func Open(db *store.Store, keyFile string) (*Vault, error) {
	if err := os.MkdirAll(filepath.Dir(keyFile), 0o700); err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(keyFile)
	if errors.Is(err, os.ErrNotExist) {
		id, err := age.GenerateX25519Identity()
		if err != nil {
			return nil, err
		}
		f, err := os.OpenFile(keyFile, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return nil, err
		}
		_, err = f.WriteString(id.String() + "\n")
		if err == nil {
			err = f.Sync()
		}
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return nil, err
		}
		return &Vault{store: db, identity: id}, nil
	}
	if err != nil {
		return nil, err
	}
	info, err := os.Lstat(keyFile)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("%s must be a regular file readable only by its owner", keyFile)
	}
	id, err := age.ParseX25519Identity(strings.TrimSpace(string(raw)))
	if err != nil {
		return nil, errors.New("invalid age identity file")
	}
	return &Vault{store: db, identity: id}, nil
}

// ValidName accepts a stack-referenceable name or registry-auth/<host>.
func ValidName(name string) bool {
	if spec.ValidName(name) {
		return true
	}
	host, ok := strings.CutPrefix(name, RegistryPrefix)
	if !ok || host == "" || len(host) > 253 || strings.ContainsAny(host, " \t\r\n\\") {
		return false
	}
	u, err := url.Parse("https://" + host)
	return err == nil && u.Host == host && u.Hostname() != "" && u.User == nil && u.Path == "" && u.RawQuery == "" && u.Fragment == ""
}

type registryAuth struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

func parseRegistryAuth(raw []byte) (registryAuth, bool) {
	var a registryAuth
	return a, json.Unmarshal(raw, &a) == nil && a.Username != "" && a.Password != ""
}

// Set encrypts and stores a value.
func (v *Vault) Set(ctx context.Context, name, value, actor string) error {
	if !ValidName(name) {
		return errors.New("secret name must be a DNS label or registry-auth/<host>")
	}
	if len(value) == 0 || len(value) > MaxValue || strings.ContainsRune(value, 0) {
		return errors.New("secret value must be 1–32768 bytes without NUL")
	}
	if strings.HasPrefix(name, RegistryPrefix) {
		if _, ok := parseRegistryAuth([]byte(value)); !ok {
			return errors.New(`registry credential must be JSON: {"username": "...", "password": "..."}`)
		}
	}
	var buf bytes.Buffer
	w, err := age.Encrypt(&buf, v.identity.Recipient())
	if err != nil {
		return errors.New("secret encryption failed")
	}
	if _, err = w.Write([]byte(value)); err != nil {
		return errors.New("secret encryption failed")
	}
	if err = w.Close(); err != nil {
		return errors.New("secret encryption failed")
	}
	return v.store.PutSecret(ctx, name, buf.Bytes(), actor)
}

func (v *Vault) decrypt(ciphertext []byte) ([]byte, error) {
	r, err := age.Decrypt(bytes.NewReader(ciphertext), v.identity)
	if err != nil {
		return nil, errors.New("secret decryption failed")
	}
	raw, err := io.ReadAll(io.LimitReader(r, MaxValue+1))
	if err != nil || len(raw) > MaxValue {
		return nil, errors.New("invalid encrypted secret")
	}
	return raw, nil
}

// Pin records, for every secret a stack references, the exact ciphertext
// in force now. Later changes to the secret do not alter this revision.
func (v *Vault) Pin(ctx context.Context, s *spec.Stack) error {
	for name, c := range s.Containers {
		c.SecretVersions = nil
		for _, ref := range c.Secrets {
			ciphertext, err := v.store.Secret(ctx, ref)
			if errors.Is(err, store.ErrNotFound) {
				return fmt.Errorf("secret %q does not exist; set it first", ref)
			}
			if err != nil {
				return err
			}
			sum := sha256.Sum256(ciphertext)
			hash := hex.EncodeToString(sum[:])
			if err = v.store.PinSecretVersion(ctx, hash, ciphertext); err != nil {
				return err
			}
			if c.SecretVersions == nil {
				c.SecretVersions = map[string]string{}
			}
			c.SecretVersions[ref] = hash
		}
		s.Containers[name] = c
	}
	return nil
}

// Version decrypts a pinned version for the runtime.
func (v *Vault) Version(ctx context.Context, hash string) ([]byte, error) {
	ciphertext, err := v.store.SecretVersion(ctx, hash)
	if err != nil {
		return nil, err
	}
	return v.decrypt(ciphertext)
}

// RegistryAuth returns the credential for an image registry host, if any.
func (v *Vault) RegistryAuth(ctx context.Context, host string) (username, password string, ok bool, err error) {
	ciphertext, err := v.store.Secret(ctx, RegistryPrefix+host)
	if errors.Is(err, store.ErrNotFound) {
		return "", "", false, nil
	}
	if err != nil {
		return "", "", false, err
	}
	raw, err := v.decrypt(ciphertext)
	if err != nil {
		return "", "", false, err
	}
	a, valid := parseRegistryAuth(raw)
	if !valid {
		return "", "", false, errors.New("invalid registry credential")
	}
	return a.Username, a.Password, true, nil
}

// matcherBudget caps the ciphertext bytes Redact keeps a matcher for. A value
// is never longer than its ciphertext, so this also caps the matcher, which
// otherwise would grow with every pinned version forever.
var matcherBudget = 2 << 20

// Redact replaces every current or pinned secret value in text. If any value
// cannot be decrypted, it withholds the text entirely.
func (v *Vault) Redact(ctx context.Context, text string) string {
	out, err := v.redact(ctx, text)
	if err != nil {
		return "[logs withheld: redaction unavailable]"
	}
	return out
}

func (v *Vault) redact(ctx context.Context, text string) (string, error) {
	all, err := v.store.AllSecretCiphertexts(ctx)
	if err != nil {
		return "", err
	}
	size := 0
	for _, ciphertext := range all {
		size += len(ciphertext)
	}
	if size > matcherBudget {
		return v.redactEach(all, text)
	}
	m, err := v.matcher(all)
	if err != nil {
		return "", err
	}
	return m.redact(text), nil
}

// matcher returns a matcher for the given ciphertexts, which Redact reads on
// every call so a value is redacted as soon as it is stored. It decrypts and
// rebuilds only when they have changed.
func (v *Vault) matcher(all [][]byte) (*matcher, error) {
	hashes := make([][32]byte, len(all))
	for i, ciphertext := range all {
		hashes[i] = sha256.Sum256(ciphertext)
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	// The ciphertexts are distinct, so the same count and no unseen hash
	// means the same set.
	same := v.redactor != nil && len(hashes) == len(v.plain)
	for _, h := range hashes {
		if _, ok := v.plain[h]; !ok {
			same = false
			break
		}
	}
	if same {
		return v.redactor, nil
	}
	plain := make(map[[32]byte][]byte, len(all))
	values := make([][]byte, 0, len(all))
	for i, ciphertext := range all {
		raw, ok := v.plain[hashes[i]]
		if !ok {
			var err error
			if raw, err = v.decrypt(ciphertext); err != nil {
				return nil, err
			}
		}
		plain[hashes[i]] = raw
		values = append(values, raw)
		if a, ok := parseRegistryAuth(raw); ok {
			values = append(values, []byte(a.Password))
		}
	}
	v.redactor, v.plain = newMatcher(values), plain
	return v.redactor, nil
}

// redactEach is the fallback past matcherBudget. It decrypts one value at a
// time and keeps none, so it is slower but holds at most one value, and it
// drops any cached matcher.
func (v *Vault) redactEach(all [][]byte, text string) (string, error) {
	v.mu.Lock()
	v.redactor, v.plain = nil, nil
	v.mu.Unlock()
	covered := make([]bool, len(text))
	for _, ciphertext := range all {
		raw, err := v.decrypt(ciphertext)
		if err != nil {
			return "", err
		}
		cover(covered, text, raw)
		if a, ok := parseRegistryAuth(raw); ok {
			cover(covered, text, []byte(a.Password))
		}
	}
	return redactCovered(text, covered), nil
}
