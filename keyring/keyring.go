// Package keyring encrypts small secrets at rest with a random key held in the
// Linux kernel user keyring, so the key lives only for the current boot and is
// readable only by the current uid. A missing key (after a reboot or Forget)
// surfaces as ErrKeyUnavailable, which callers treat as "log in again".
package keyring

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
)

var (
	// ErrKeyUnavailable means the key is missing, revoked or not the one the
	// data was sealed with.
	ErrKeyUnavailable = errors.New("keyring: key unavailable")
	// ErrCorrupt means the envelope is malformed or fails authentication.
	ErrCorrupt = errors.New("keyring: sealed data corrupt")
)

const (
	keySize   = 32
	version1  = 1
	fpSize    = 8
	nonceSize = 12
	headerLen = 1 + fpSize + nonceSize
)

func fingerprint(key []byte) []byte {
	sum := sha256.Sum256(key)
	return sum[:fpSize]
}

// seal returns version || fingerprint || nonce || AES-256-GCM(plaintext), with
// version and fingerprint authenticated as additional data.
func seal(key, plaintext []byte) ([]byte, error) {
	gcm, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	out := make([]byte, headerLen, headerLen+len(plaintext)+gcm.Overhead())
	out[0] = version1
	copy(out[1:], fingerprint(key))
	if _, err := rand.Read(out[1+fpSize : headerLen]); err != nil {
		return nil, err
	}
	return gcm.Seal(out, out[1+fpSize:headerLen], plaintext, out[:1+fpSize]), nil
}

func unseal(key, sealed []byte) ([]byte, error) {
	if len(sealed) < headerLen || sealed[0] != version1 {
		return nil, ErrCorrupt
	}
	if subtle.ConstantTimeCompare(sealed[1:1+fpSize], fingerprint(key)) != 1 {
		return nil, ErrKeyUnavailable
	}
	gcm, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	pt, err := gcm.Open(nil, sealed[1+fpSize:headerLen], sealed[headerLen:], sealed[:1+fpSize])
	if err != nil {
		return nil, ErrCorrupt
	}
	return pt, nil
}

func newGCM(key []byte) (cipher.AEAD, error) {
	if len(key) != keySize {
		return nil, ErrKeyUnavailable
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}
