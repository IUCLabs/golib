//go:build !linux

package keyring

import (
	"errors"
	"fmt"
)

var errUnsupported = fmt.Errorf("keyring: kernel keyring requires linux: %w", errors.ErrUnsupported)

// Open always fails: the kernel key retention service is Linux-only.
func Open(string) (*Keyring, error) { return nil, errUnsupported }

// Seal always fails on this platform.
func (*Keyring) Seal([]byte) ([]byte, error) { return nil, errUnsupported }

// Unseal always fails on this platform.
func (*Keyring) Unseal([]byte) ([]byte, error) { return nil, errUnsupported }

// Forget always fails on this platform.
func (*Keyring) Forget() error { return errUnsupported }
