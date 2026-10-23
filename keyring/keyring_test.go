package keyring

import (
	"bytes"
	"errors"
	"testing"
)

func testKey(b byte) []byte { return bytes.Repeat([]byte{b}, keySize) }

func TestSealUnsealRoundTrip(t *testing.T) {
	for _, pt := range [][]byte{nil, []byte("hvs.token"), bytes.Repeat([]byte("x"), 4096)} {
		sealed, err := seal(testKey(1), pt)
		if err != nil {
			t.Fatal(err)
		}
		got, err := unseal(testKey(1), sealed)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, pt) {
			t.Fatalf("got %q, want %q", got, pt)
		}
	}
}

func TestSealUsesFreshNonce(t *testing.T) {
	a, _ := seal(testKey(1), []byte("same"))
	b, _ := seal(testKey(1), []byte("same"))
	if bytes.Equal(a, b) {
		t.Fatal("two seals of the same plaintext are identical")
	}
}

func TestUnsealWrongKey(t *testing.T) {
	sealed, _ := seal(testKey(1), []byte("secret"))
	if _, err := unseal(testKey(2), sealed); !errors.Is(err, ErrKeyUnavailable) {
		t.Fatalf("err = %v, want ErrKeyUnavailable", err)
	}
}

func TestUnsealTampered(t *testing.T) {
	sealed, _ := seal(testKey(1), []byte("secret"))
	cases := map[string]func([]byte) []byte{
		"ciphertext": func(b []byte) []byte { b[len(b)-1] ^= 1; return b },
		"nonce":      func(b []byte) []byte { b[1+fpSize] ^= 1; return b },
		"version":    func(b []byte) []byte { b[0] = 2; return b },
		"truncated":  func(b []byte) []byte { return b[:headerLen-1] },
		"empty":      func([]byte) []byte { return nil },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			b := mutate(bytes.Clone(sealed))
			if _, err := unseal(testKey(1), b); !errors.Is(err, ErrCorrupt) {
				t.Fatalf("err = %v, want ErrCorrupt", err)
			}
		})
	}
}

func TestBadKeyLength(t *testing.T) {
	if _, err := seal([]byte("short"), []byte("x")); !errors.Is(err, ErrKeyUnavailable) {
		t.Fatalf("seal err = %v, want ErrKeyUnavailable", err)
	}
	short := []byte("short")
	sealed, _ := seal(testKey(1), []byte("x"))
	copy(sealed[1:], fingerprint(short))
	if _, err := unseal(short, sealed); !errors.Is(err, ErrKeyUnavailable) {
		t.Fatalf("unseal err = %v, want ErrKeyUnavailable", err)
	}
}
