// Package secretbox encrypts small secrets (such as a Nextcloud token) before they are stored in the document store.
package secretbox

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"strings"
)

type Box struct{ aead cipher.AEAD }

// New derives an AES-256-GCM key from any secret string.
func New(secret string) (*Box, error) {
	if secret == "" {
		return nil, errors.New("empty secret")
	}
	k := sha256.Sum256([]byte("lumen-secretbox-v1:" + secret))
	blk, err := aes.NewCipher(k[:])
	if err != nil {
		return nil, err
	}
	a, err := cipher.NewGCM(blk)
	return &Box{a}, err
}

// Seal returns "v1:<base64>" (empty in, empty out).
func (b *Box) Seal(plain string) string {
	if plain == "" {
		return ""
	}
	n := make([]byte, b.aead.NonceSize())
	if _, err := rand.Read(n); err != nil {
		panic("no randomness available")
	}
	return "v1:" + base64.RawStdEncoding.EncodeToString(b.aead.Seal(n, n, []byte(plain), nil))
}

// Open reverses Seal. It fails if the key is wrong or the value was changed.
func (b *Box) Open(sealed string) (string, error) {
	if sealed == "" {
		return "", nil
	}
	raw, ok := strings.CutPrefix(sealed, "v1:")
	if !ok {
		return "", errors.New("unknown format")
	}
	d, err := base64.RawStdEncoding.DecodeString(raw)
	if err != nil || len(d) < b.aead.NonceSize() {
		return "", errors.New("corrupt value")
	}
	p, err := b.aead.Open(nil, d[:b.aead.NonceSize()], d[b.aead.NonceSize():], nil)
	if err != nil {
		return "", errors.New("cannot decrypt (was the secret key changed?)")
	}
	return string(p), nil
}
