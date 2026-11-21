package main

import (
	"fmt"

	"golang.org/x/sys/unix"
)

func main() {
	for _, s := range []struct {
		n  string
		id int
	}{{"thread", unix.KEY_SPEC_THREAD_KEYRING}, {"session", unix.KEY_SPEC_SESSION_KEYRING}, {"user", unix.KEY_SPEC_USER_KEYRING}} {
		id, err := unix.KeyctlGetKeyringID(s.id, true)
		fmt.Println("get", s.n, id, err)
	}
	id, err := unix.AddKey("user", "diag", []byte("x"), unix.KEY_SPEC_THREAD_KEYRING)
	fmt.Println("addkey thread", id, err)
	_, err = unix.KeyctlSearch(unix.KEY_SPEC_USER_KEYRING, "user", "nope", 0)
	fmt.Println("search user", err)
	_, err = unix.KeyctlJoinSessionKeyring("")
	fmt.Println("join session", err)
	_, err = unix.KeyctlSearch(unix.KEY_SPEC_USER_KEYRING, "user", "nope", 0)
	fmt.Println("search user after join", err)
}
