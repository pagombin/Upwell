// Package secrets encrypts credentials at rest with AES-256-GCM and a local
// 256-bit key file.
package secrets

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/pagombin/upwell/internal/store"
)

// Box seals and opens secrets.
type Box struct {
	aead    cipher.AEAD
	version int
}

// LoadOrCreateKey reads the hex key at path, creating it (mode 0600) when
// missing and create is true.
func LoadOrCreateKey(path string, create bool) ([]byte, error) {
	b, err := os.ReadFile(path)
	if err == nil {
		key, err := hex.DecodeString(strings.TrimSpace(string(b)))
		if err != nil || len(key) != 32 {
			return nil, fmt.Errorf("master key %s is not 32 hex-encoded bytes", path)
		}
		return key, nil
	}
	if !os.IsNotExist(err) || !create {
		return nil, fmt.Errorf("master key %s: %w", path, err)
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return nil, err
	}
	if err := os.WriteFile(path, []byte(hex.EncodeToString(key)+"\n"), 0o600); err != nil {
		return nil, err
	}
	return key, nil
}

// New returns a Box for key.
func New(key []byte) (*Box, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Box{aead: aead, version: 1}, nil
}

// Seal encrypts plaintext with a fresh nonce.
func (b *Box) Seal(plaintext []byte, name string) (nonce, ct []byte, err error) {
	nonce = make([]byte, b.aead.NonceSize())
	if _, err = rand.Read(nonce); err != nil {
		return nil, nil, err
	}
	return nonce, b.aead.Seal(nil, nonce, plaintext, []byte(name)), nil
}

// Open decrypts.
func (b *Box) Open(nonce, ct []byte, name string) ([]byte, error) {
	return b.aead.Open(nil, nonce, ct, []byte(name))
}

// Vault stores secrets in the store, encrypted.
type Vault struct {
	Box   *Box
	Store *store.Store
}

// Put stores a secret and returns its id.
func (v *Vault) Put(ctx context.Context, name, value string) (string, error) {
	nonce, ct, err := v.Box.Seal([]byte(value), name)
	if err != nil {
		return "", err
	}
	id := store.NewID()
	_, err = v.Store.DB.ExecContext(ctx, `INSERT INTO secrets(id,name,ciphertext,nonce,key_version,created_at) VALUES (?,?,?,?,?,?)`,
		id, name, ct, nonce, v.Box.version, store.Now())
	return id, err
}

// Get returns a secret's plaintext.
func (v *Vault) Get(ctx context.Context, id string) (string, error) {
	var name string
	var ct, nonce []byte
	err := v.Store.DB.QueryRowContext(ctx, `SELECT name, ciphertext, nonce FROM secrets WHERE id=?`, id).Scan(&name, &ct, &nonce)
	if errors.Is(err, sql.ErrNoRows) {
		return "", store.ErrNotFound
	}
	if err != nil {
		return "", err
	}
	pt, err := v.Box.Open(nonce, ct, name)
	if err != nil {
		return "", fmt.Errorf("secret %s cannot be decrypted with this master key", name)
	}
	return string(pt), nil
}

// Delete removes a secret.
func (v *Vault) Delete(ctx context.Context, id string) error {
	_, err := v.Store.DB.ExecContext(ctx, `DELETE FROM secrets WHERE id=?`, id)
	return err
}

// Canary checks that the key decrypts an existing secret, or creates a canary.
func (v *Vault) Canary(ctx context.Context) error {
	var id string
	err := v.Store.DB.QueryRowContext(ctx, `SELECT id FROM secrets WHERE name='canary'`).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		_, err = v.Put(ctx, "canary", "upwell")
		return err
	}
	if err != nil {
		return err
	}
	got, err := v.Get(ctx, id)
	if err != nil {
		return err
	}
	if got != "upwell" {
		return errors.New("canary secret does not match")
	}
	return nil
}

// Rotate re-encrypts every secret with newBox.
func (v *Vault) Rotate(ctx context.Context, newBox *Box) error {
	rows, err := v.Store.DB.QueryContext(ctx, `SELECT id, name, ciphertext, nonce FROM secrets`)
	if err != nil {
		return err
	}
	type rec struct {
		id, name  string
		ct, nonce []byte
	}
	var all []rec
	for rows.Next() {
		var r rec
		if err := rows.Scan(&r.id, &r.name, &r.ct, &r.nonce); err != nil {
			rows.Close()
			return err
		}
		all = append(all, r)
	}
	rows.Close()
	tx, err := v.Store.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, r := range all {
		pt, err := v.Box.Open(r.nonce, r.ct, r.name)
		if err != nil {
			return fmt.Errorf("secret %s: %w", r.name, err)
		}
		nonce, ct, err := newBox.Seal(pt, r.name)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE secrets SET ciphertext=?, nonce=?, key_version=key_version+1 WHERE id=?`, ct, nonce, r.id); err != nil {
			return err
		}
	}
	return tx.Commit()
}
