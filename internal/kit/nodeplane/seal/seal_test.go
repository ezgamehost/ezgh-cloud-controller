package seal

import (
	"bytes"
	"crypto/ecdh"
	"crypto/rand"
	"testing"
)

const (
	info  = "ezgh-test-sealed-v1"
	scope = "dbi_abcdefghijk2"
)

func TestSealBindsScopeNameVersion(t *testing.T) {
	key, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	private, public := key.Bytes(), key.PublicKey().Bytes()
	plaintext := []byte("a replication password")
	sealed, err := Seal(info, public, scope, "replication", 2, plaintext)
	if err != nil {
		t.Fatal(err)
	}
	got, err := Open(info, private, scope, "replication", 2, sealed)
	if err != nil || !bytes.Equal(got, plaintext) {
		t.Fatalf("%q %v", got, err)
	}
	if _, err := Open("ezgh-other-sealed-v1", private, scope, "replication", 2, sealed); err == nil {
		t.Error("another info opened it")
	}
	if _, err := Open(info, private, "dbi_abcdefghijk3", "replication", 2, sealed); err == nil {
		t.Error("another scope opened it")
	}
	if _, err := Open(info, private, scope, "admin", 2, sealed); err == nil {
		t.Error("another name opened it")
	}
	if _, err := Open(info, private, scope, "replication", 3, sealed); err == nil {
		t.Error("another version opened it")
	}
	tampered := bytes.Clone(sealed)
	tampered[len(tampered)-1] ^= 1
	if _, err := Open(info, private, scope, "replication", 2, tampered); err == nil {
		t.Error("tampered ciphertext opened")
	}
	if _, err := Open(info, private, scope, "replication", 2, sealed[:31]); err == nil {
		t.Error("a truncated sealed value opened")
	}
	if _, err := Seal(info, []byte{1, 2}, scope, "replication", 2, plaintext); err == nil {
		t.Error("sealed to a key that isn't one")
	}
}
