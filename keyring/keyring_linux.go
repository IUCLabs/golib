package keyring

import (
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"runtime"

	"golang.org/x/sys/unix"
)

// Possessor: all. User (same uid): view, read, write, search, setattr. Granting
// the uid matters because the user keyring is only possessed by processes whose
// session keyring links it, which a systemd user service or a `keyctl session`
// shell does not.
const keyPerm = 0x3f000000 | 0x002f0000

const createAttempts = 5

// Keyring seals and unseals data with the kernel-held key named by its
// description.
type Keyring struct {
	desc string
}

// Open returns the keyring for desc, creating its key in the user keyring
// (KEY_SPEC_USER_KEYRING, shared by every session of this uid until reboot) if
// absent.
func Open(desc string) (*Keyring, error) {
	if desc == "" {
		return nil, errors.New("keyring: empty description")
	}
	k := &Keyring{desc: desc}
	if _, err := k.getOrCreate(); err != nil {
		return nil, err
	}
	return k, nil
}

// Seal encrypts plaintext, recreating the key if it has been dropped.
func (k *Keyring) Seal(plaintext []byte) ([]byte, error) {
	key, err := k.getOrCreate()
	if err != nil {
		return nil, err
	}
	return seal(key, plaintext)
}

// Unseal decrypts data from Seal. It returns ErrKeyUnavailable when the key is
// gone or differs from the sealing key, and ErrCorrupt when data is damaged.
func (k *Keyring) Unseal(sealed []byte) ([]byte, error) {
	_, key, err := k.lookup()
	if err != nil {
		return nil, err
	}
	return unseal(key, sealed)
}

// Forget revokes the key and unlinks it from the user keyring, so everything
// sealed with it becomes unreadable. Forgetting an absent key is not an error.
func (k *Keyring) Forget() error {
	id, err := unix.KeyctlSearch(unix.KEY_SPEC_USER_KEYRING, "user", k.desc, 0)
	if err != nil {
		if missing(err) {
			return nil
		}
		return fmt.Errorf("keyring: search %q: %w", k.desc, err)
	}
	if _, err := unix.KeyctlInt(unix.KEYCTL_REVOKE, id, 0, 0, 0); err != nil {
		return fmt.Errorf("keyring: revoke %q: %w", k.desc, err)
	}
	unlink(id)
	return nil
}

// lookup returns the key and its id; the id is non-zero whenever the search
// found a key, even one that turned out unusable.
func (k *Keyring) lookup() (int, []byte, error) {
	id, err := unix.KeyctlSearch(unix.KEY_SPEC_USER_KEYRING, "user", k.desc, 0)
	if err != nil {
		if missing(err) {
			return 0, nil, fmt.Errorf("%w: %w", ErrKeyUnavailable, err)
		}
		return 0, nil, fmt.Errorf("keyring: search %q: %w", k.desc, err)
	}
	buf := make([]byte, 2*keySize)
	n, err := unix.KeyctlBuffer(unix.KEYCTL_READ, id, buf, 0)
	if err != nil {
		if missing(err) {
			return id, nil, fmt.Errorf("%w: %v", ErrKeyUnavailable, err)
		}
		return id, nil, fmt.Errorf("keyring: read %q: %w", k.desc, err)
	}
	if n != keySize {
		return id, nil, fmt.Errorf("%w: key %q is %d bytes", ErrKeyUnavailable, k.desc, n)
	}
	return id, buf[:n], nil
}

// getOrCreate converges concurrent first users on one key: create links
// exclusively, so a loser gets EEXIST and re-reads the winner's key.
func (k *Keyring) getOrCreate() ([]byte, error) {
	for range createAttempts {
		id, key, err := k.lookup()
		if !errors.Is(err, ErrKeyUnavailable) {
			return key, err
		}
		// An unusable key of our description blocks the exclusive link.
		if id != 0 {
			unlink(id)
		} else if errors.Is(err, unix.EKEYREVOKED) || errors.Is(err, unix.EKEYEXPIRED) {
			purgeDead()
		}
		key, err = k.create()
		if !errors.Is(err, unix.EEXIST) {
			return key, err
		}
	}
	return nil, fmt.Errorf("keyring: %q: key creation kept racing", k.desc)
}

// create adds a fresh key to this thread's keyring, which is always possessed,
// so setperm succeeds, then moves it into the user keyring with MOVE_EXCL,
// failing with EEXIST if a key of the same description is already there.
func (k *Keyring) create() ([]byte, error) {
	key := make([]byte, keySize)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	id, err := unix.AddKey("user", k.desc, key, unix.KEY_SPEC_THREAD_KEYRING)
	if err != nil {
		return nil, fmt.Errorf("keyring: add %q: %w", k.desc, err)
	}
	defer func() { _, _ = unix.KeyctlInt(unix.KEYCTL_UNLINK, id, unix.KEY_SPEC_THREAD_KEYRING, 0, 0) }()
	if err := unix.KeyctlSetperm(id, keyPerm); err != nil {
		return nil, fmt.Errorf("keyring: setperm %q: %w", k.desc, err)
	}
	if _, err := unix.KeyctlInt(unix.KEYCTL_MOVE, id, unix.KEY_SPEC_THREAD_KEYRING, unix.KEY_SPEC_USER_KEYRING, unix.KEYCTL_MOVE_EXCL); err != nil {
		if errors.Is(err, unix.EEXIST) {
			return nil, err
		}
		return nil, fmt.Errorf("keyring: move %q: %w", k.desc, err)
	}
	return key, nil
}

// purgeDead unlinks every revoked or expired key in the user keyring: a dead key
// cannot be described, so ours cannot be singled out. Call it only after the
// search hit a dead key of our description.
func purgeDead() {
	buf := make([]byte, 256)
	for {
		n, err := unix.KeyctlBuffer(unix.KEYCTL_READ, unix.KEY_SPEC_USER_KEYRING, buf, 0)
		if err != nil {
			return
		}
		if n <= len(buf) {
			buf = buf[:n]
			break
		}
		buf = make([]byte, n)
	}
	for ; len(buf) >= 4; buf = buf[4:] {
		id := int(int32(binary.NativeEndian.Uint32(buf)))
		if _, err := unix.KeyctlString(unix.KEYCTL_DESCRIBE, id); missing(err) {
			unlink(id)
		}
	}
}

func unlink(id int) {
	_, _ = unix.KeyctlInt(unix.KEYCTL_UNLINK, id, unix.KEY_SPEC_USER_KEYRING, 0, 0)
}

func missing(err error) bool {
	return errors.Is(err, unix.ENOKEY) || errors.Is(err, unix.EKEYREVOKED) || errors.Is(err, unix.EKEYEXPIRED)
}
