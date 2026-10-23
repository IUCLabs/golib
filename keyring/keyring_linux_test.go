package keyring

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"runtime"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

// open opens a test-unique keyring, skipping where the container's seccomp
// profile blocks the keyctl syscalls.
func open(t *testing.T) *Keyring {
	t.Helper()
	k, err := Open(fmt.Sprintf("golib-test:%s:%d", t.Name(), os.Getpid()))
	if errors.Is(err, unix.ENOSYS) || errors.Is(err, unix.EPERM) || errors.Is(err, unix.EACCES) {
		t.Skipf("kernel keyring unavailable: %v", err)
	}
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = k.Forget() })
	return k
}

func TestKernelRoundTrip(t *testing.T) {
	k := open(t)
	sealed, err := k.Seal([]byte("hvs.token"))
	if err != nil {
		t.Fatal(err)
	}
	k2, err := Open(k.desc)
	if err != nil {
		t.Fatal(err)
	}
	got, err := k2.Unseal(sealed)
	if err != nil || string(got) != "hvs.token" {
		t.Fatalf("Unseal = %q, %v", got, err)
	}
}

func TestKernelKeyPermissions(t *testing.T) {
	k := open(t)
	id, err := unix.KeyctlSearch(unix.KEY_SPEC_USER_KEYRING, "user", k.desc, 0)
	if err != nil {
		t.Fatal(err)
	}
	desc, err := unix.KeyctlString(unix.KEYCTL_DESCRIBE, id)
	if err != nil {
		t.Fatal(err)
	}
	if f := strings.Split(desc, ";"); len(f) < 4 || f[3] != "3f2f0000" {
		t.Fatalf("describe = %q, want perm 3f2f0000", desc)
	}
}

// A process whose session keyring does not link the user keyring (a systemd
// user service, a fresh `keyctl session`) must still read the key.
func TestKernelUnsealWithoutPossession(t *testing.T) {
	k := open(t)
	sealed, err := k.Seal([]byte("secret"))
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error)
	go func() {
		runtime.LockOSThread() // never unlocked: the thread exits with its new session
		if _, err := unix.KeyctlInt(unix.KEYCTL_JOIN_SESSION_KEYRING, 0, 0, 0, 0); err != nil {
			done <- err
			return
		}
		got, err := k.Unseal(sealed)
		if err == nil && !bytes.Equal(got, []byte("secret")) {
			err = fmt.Errorf("got %q", got)
		}
		done <- err
	}()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestKernelForget(t *testing.T) {
	k := open(t)
	sealed, err := k.Seal([]byte("secret"))
	if err != nil {
		t.Fatal(err)
	}
	if err := k.Forget(); err != nil {
		t.Fatal(err)
	}
	if _, err := k.Unseal(sealed); !errors.Is(err, ErrKeyUnavailable) {
		t.Fatalf("Unseal after Forget: err = %v, want ErrKeyUnavailable", err)
	}
	if err := k.Forget(); err != nil {
		t.Fatalf("second Forget: %v", err)
	}
	resealed, err := k.Seal([]byte("again"))
	if err != nil {
		t.Fatalf("Seal after Forget: %v", err)
	}
	if _, err := k.Unseal(sealed); !errors.Is(err, ErrKeyUnavailable) {
		t.Fatalf("old data under new key: err = %v, want ErrKeyUnavailable", err)
	}
	if got, err := k.Unseal(resealed); err != nil || string(got) != "again" {
		t.Fatalf("Unseal = %q, %v", got, err)
	}
}

func TestKernelWrongSizedKeyIsReplaced(t *testing.T) {
	k := open(t)
	sealed, _ := k.Seal([]byte("secret"))
	if _, err := unix.AddKey("user", k.desc, []byte("short"), unix.KEY_SPEC_USER_KEYRING); err != nil {
		t.Fatal(err)
	}
	if _, err := k.Unseal(sealed); !errors.Is(err, ErrKeyUnavailable) {
		t.Fatalf("err = %v, want ErrKeyUnavailable", err)
	}
	if _, err := Open(k.desc); err != nil {
		t.Fatal(err)
	}
	if _, err := k.get(); err != nil {
		t.Fatalf("key not replaced: %v", err)
	}
}

func TestOpenEmptyDescription(t *testing.T) {
	if _, err := Open(""); err == nil {
		t.Fatal("Open(\"\") succeeded")
	}
}
