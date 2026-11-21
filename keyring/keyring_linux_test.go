package keyring

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"runtime"
	"strings"
	"sync"
	"testing"

	"golang.org/x/sys/unix"
)

// open opens a test-unique keyring, skipping outside CI where keyctl is
// unimplemented, and wherever a seccomp filter denies keyctl outright (the
// thread keyring is always accessible otherwise). Other permission errors
// always fail: they are bugs, not absence.
func open(t *testing.T) *Keyring {
	t.Helper()
	if _, err := unix.KeyctlGetKeyringID(unix.KEY_SPEC_THREAD_KEYRING, true); errors.Is(err, unix.EPERM) {
		t.Skipf("keyctl denied by sandbox: %v", err)
	}
	k, err := Open(fmt.Sprintf("golib-test:%s:%d", t.Name(), os.Getpid()))
	if errors.Is(err, unix.ENOSYS) && os.Getenv("CI") == "" {
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
	sealed, err := k.Seal([]byte("secret"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := unix.AddKey("user", k.desc, []byte("short"), unix.KEY_SPEC_USER_KEYRING); err != nil {
		t.Fatal(err)
	}
	if _, err := k.Unseal(sealed); !errors.Is(err, ErrKeyUnavailable) {
		t.Fatalf("err = %v, want ErrKeyUnavailable", err)
	}
	if _, err := Open(k.desc); err != nil {
		t.Fatal(err)
	}
	if _, _, err := k.lookup(); err != nil {
		t.Fatalf("key not replaced: %v", err)
	}
}

// Racing first users must all end up sealing under the one key that stays linked.
func TestKernelConcurrentFirstSeal(t *testing.T) {
	open(t) // skips where keyctl is unavailable
	const rounds, workers = 20, 8
	for r := range rounds {
		desc := fmt.Sprintf("golib-test:%s:%d:%d", t.Name(), os.Getpid(), r)
		sealed := make([][]byte, workers)
		errs := make([]error, workers)
		var wg sync.WaitGroup
		for w := range workers {
			wg.Go(func() {
				k, err := Open(desc)
				if err == nil {
					sealed[w], err = k.Seal([]byte("secret"))
				}
				errs[w] = err
			})
		}
		wg.Wait()
		k := &Keyring{desc: desc}
		t.Cleanup(func() { _ = k.Forget() })
		for w := range workers {
			if errs[w] != nil {
				t.Fatalf("round %d worker %d: %v", r, w, errs[w])
			}
			if got, err := k.Unseal(sealed[w]); err != nil || string(got) != "secret" {
				t.Fatalf("round %d worker %d: Unseal = %q, %v", r, w, got, err)
			}
		}
	}
}

// A key revoked behind our back still blocks an exclusive link until unlinked.
func TestKernelExternallyRevokedKeyIsReplaced(t *testing.T) {
	k := open(t)
	id, _, err := k.lookup()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := unix.KeyctlInt(unix.KEYCTL_REVOKE, id, 0, 0, 0); err != nil {
		t.Fatal(err)
	}
	sealed, err := k.Seal([]byte("secret"))
	if err != nil {
		t.Fatalf("Seal after external revoke: %v", err)
	}
	if got, err := k.Unseal(sealed); err != nil || string(got) != "secret" {
		t.Fatalf("Unseal = %q, %v", got, err)
	}
}

// Another application's dead key in the user keyring is not ours to unlink.
func TestKernelForeignDeadKeySurvivesOpen(t *testing.T) {
	open(t) // skips where keyctl is unavailable
	foreign := &Keyring{desc: fmt.Sprintf("golib-test-foreign:%s:%d", t.Name(), os.Getpid())}
	if _, err := foreign.create(); err != nil {
		t.Fatal(err)
	}
	id, _, err := foreign.lookup()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { unlink(id) })
	if _, err := unix.KeyctlInt(unix.KEYCTL_REVOKE, id, 0, 0, 0); err != nil {
		t.Fatal(err)
	}
	k, err := Open(fmt.Sprintf("golib-test:%s:%d:fresh", t.Name(), os.Getpid()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = k.Forget() })
	// Unlinking an id that is no longer linked fails with ENOENT.
	if _, err := unix.KeyctlInt(unix.KEYCTL_UNLINK, id, unix.KEY_SPEC_USER_KEYRING, 0, 0); err != nil {
		t.Fatalf("foreign dead key was unlinked: %v", err)
	}
}

func TestOpenEmptyDescription(t *testing.T) {
	if _, err := Open(""); err == nil {
		t.Fatal("Open(\"\") succeeded")
	}
}
