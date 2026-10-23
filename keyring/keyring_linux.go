package keyring

import (
	"crypto/rand"
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
	key, err := k.get()
	if err != nil {
		return nil, err
	}
	return unseal(key, sealed)
}

// Forget revokes the key, so everything sealed with it becomes unreadable.
// Forgetting an absent key is not an error.
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
	return nil
}

func (k *Keyring) get() ([]byte, error) {
	id, err := unix.KeyctlSearch(unix.KEY_SPEC_USER_KEYRING, "user", k.desc, 0)
	if err != nil {
		if missing(err) {
			return nil, fmt.Errorf("%w: %v", ErrKeyUnavailable, err)
		}
		return nil, fmt.Errorf("keyring: search %q: %w", k.desc, err)
	}
	buf := make([]byte, 2*keySize)
	n, err := unix.KeyctlBuffer(unix.KEYCTL_READ, id, buf, 0)
	if err != nil {
		if missing(err) {
			return nil, fmt.Errorf("%w: %v", ErrKeyUnavailable, err)
		}
		return nil, fmt.Errorf("keyring: read %q: %w", k.desc, err)
	}
	if n != keySize {
		return nil, fmt.Errorf("%w: key %q is %d bytes", ErrKeyUnavailable, k.desc, n)
	}
	return buf[:n], nil
}

func (k *Keyring) getOrCreate() ([]byte, error) {
	key, err := k.get()
	if !errors.Is(err, ErrKeyUnavailable) {
		return key, err
	}
	if err := k.create(); err != nil {
		return nil, err
	}
	// Re-read so concurrent creators converge on whichever key was linked last.
	return k.get()
}

// create adds the key to this thread's keyring, which is always possessed, so
// setperm succeeds; linking it into the user keyring then replaces any stale
// key of the same description.
func (k *Keyring) create() error {
	key := make([]byte, keySize)
	if _, err := rand.Read(key); err != nil {
		return err
	}
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	id, err := unix.AddKey("user", k.desc, key, unix.KEY_SPEC_THREAD_KEYRING)
	if err != nil {
		return fmt.Errorf("keyring: add %q: %w", k.desc, err)
	}
	defer func() { _, _ = unix.KeyctlInt(unix.KEYCTL_UNLINK, id, unix.KEY_SPEC_THREAD_KEYRING, 0, 0) }()
	if err := unix.KeyctlSetperm(id, keyPerm); err != nil {
		return fmt.Errorf("keyring: setperm %q: %w", k.desc, err)
	}
	if _, err := unix.KeyctlInt(unix.KEYCTL_LINK, id, unix.KEY_SPEC_USER_KEYRING, 0, 0); err != nil {
		return fmt.Errorf("keyring: link %q: %w", k.desc, err)
	}
	return nil
}

func missing(err error) bool {
	return errors.Is(err, unix.ENOKEY) || errors.Is(err, unix.EKEYREVOKED) || errors.Is(err, unix.EKEYEXPIRED)
}
