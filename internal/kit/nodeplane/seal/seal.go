// Package seal encrypts a secret to one node's X25519 key, sent over a node plane's stream: HPKE
// base mode with DHKEM(X25519, HKDF-SHA256), HKDF-SHA256 and AES-256-GCM. The node's key is its
// raw public key from the stream's hello, not its mTLS key, trusted because it arrives over the
// authenticated stream.
//
// info names the product and purpose (such as "ezgh-databases-sealed-v1"); the additional data ties
// a value to one scope (the resource it belongs to), name and version, so it can't be replayed onto
// another.
package seal

import (
	"crypto/ecdh"
	"crypto/hpke"
	"errors"
	"fmt"
	"strconv"
)

func suite() (hpke.KEM, hpke.KDF, hpke.AEAD) {
	return hpke.DHKEM(ecdh.X25519()), hpke.HKDFSHA256(), hpke.AES256GCM()
}

func aad(scope, name string, version int64) []byte {
	return []byte(scope + "\n" + name + "\n" + strconv.FormatInt(version, 10))
}

// Seal encrypts plaintext to recipient, a raw X25519 public key: enc || ciphertext.
func Seal(info string, recipient []byte, scope, name string, version int64, plaintext []byte) ([]byte, error) {
	kem, kdf, aead := suite()
	public, err := kem.NewPublicKey(recipient)
	if err != nil {
		return nil, fmt.Errorf("seal: the seal key: %w", err)
	}
	enc, sender, err := hpke.NewSender(public, kdf, aead, []byte(info))
	if err != nil {
		return nil, err
	}
	ciphertext, err := sender.Seal(aad(scope, name, version), plaintext)
	if err != nil {
		return nil, err
	}
	return append(enc, ciphertext...), nil
}

// Open decrypts what Seal made for private, a raw X25519 private key. It fails on another info,
// scope, name or version as on any tampering.
func Open(info string, private []byte, scope, name string, version int64, sealed []byte) ([]byte, error) {
	kem, kdf, aead := suite()
	key, err := kem.NewPrivateKey(private)
	if err != nil {
		return nil, fmt.Errorf("seal: the seal key: %w", err)
	}
	const encLen = 32 // X25519's encapsulated key
	if len(sealed) < encLen {
		return nil, errors.New("seal: a sealed value too short to hold its key")
	}
	recipient, err := hpke.NewRecipient(sealed[:encLen], key, kdf, aead, []byte(info))
	if err != nil {
		return nil, err
	}
	return recipient.Open(aad(scope, name, version), sealed[encLen:])
}
