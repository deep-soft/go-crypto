package openpgp

import (
	"bytes"
	"crypto"
	"strings"
	"testing"

	"github.com/ProtonMail/go-crypto/openpgp/packet"
)

var foreignKeysV6 = []string{
	v6PrivKey,
}

func TestReadPrivateForeignV6Key(t *testing.T) {
	for _, str := range foreignKeysV6 {
		kring, err := ReadArmoredKeyRing(strings.NewReader(str))
		if err != nil {
			t.Fatal(err)
		}
		checkV6Key(t, kring[0])
	}
}

func TestReadPrivateEncryptedV6Key(t *testing.T) {
	c := &packet.Config{V6Keys: true}
	e, err := NewEntity("V6 Key Owner", "V6 Key", "v6@pm.me", c)
	if err != nil {
		t.Fatal(err)
	}
	password := []byte("test v6 key # password")
	// Encrypt private key
	if err = e.PrivateKey.Encrypt(password); err != nil {
		t.Fatal(err)
	}
	// Encrypt subkeys
	for _, sub := range e.Subkeys {
		if err = sub.PrivateKey.Encrypt(password); err != nil {
			t.Fatal(err)
		}
	}
	// Serialize, Read
	serializedEntity := bytes.NewBuffer(nil)
	err = e.SerializePrivateWithoutSigning(serializedEntity, nil)
	if err != nil {
		t.Fatal(err)
	}
	el, err := ReadKeyRing(serializedEntity)
	if err != nil {
		t.Fatal(err)
	}

	// Decrypt
	if el[0].PrivateKey == nil {
		t.Fatal("No private key found")
	}
	if err = el[0].PrivateKey.Decrypt(password); err != nil {
		t.Error(err)
	}

	// Decrypt subkeys
	for _, sub := range e.Subkeys {
		if err = sub.PrivateKey.Decrypt(password); err != nil {
			t.Error(err)
		}
	}

	checkV6Key(t, el[0])
}

func TestNewEntitySerializeV6Key(t *testing.T) {
	c := &packet.Config{V6Keys: true}
	e, err := NewEntity("V6 Key Owner", "V6 Key", "v6@pm.me", c)
	if err != nil {
		t.Fatal(err)
	}
	checkSerializeReadv6(t, e)
}

func TestNewEntityV6Key(t *testing.T) {
	c := &packet.Config{
		V6Keys: true,
	}
	e, err := NewEntity("V6 Key Owner", "V6 Key", "v6@pm.me", c)
	if err != nil {
		t.Fatal(err)
	}
	checkV6Key(t, e)
}

func checkV6Key(t *testing.T, ent *Entity) {
	key := ent.PrimaryKey
		if key.Version != 6 {
			t.Errorf("wrong key version %d", key.Version)
		}
		if len(key.Fingerprint) != 32 {
			t.Errorf("Wrong fingerprint length: %d", len(key.Fingerprint))
		}
	signatures := ent.Revocations
	for _, id := range ent.Identities {
		signatures = append(signatures, id.SelfSignature)
		signatures = append(signatures, id.Signatures...)
	}
	for _, sig := range signatures {
		if sig == nil {
			continue
		}
		if sig.Version != 6 {
			t.Errorf("wrong signature version %d", sig.Version)
		}
		fgptLen := len(sig.IssuerFingerprint)
		if fgptLen!= 32 {
			t.Errorf("Wrong fingerprint length in signature: %d", fgptLen)
		}
	}
}

func checkSerializeReadv6(t *testing.T, e *Entity) {
	// Entity serialize
	serializedEntity := bytes.NewBuffer(nil)
	err := e.Serialize(serializedEntity)
	if err != nil {
		t.Fatal(err)
	}
	el, err := ReadKeyRing(serializedEntity)
	if err != nil {
		t.Fatal(err)
	}
	checkV6Key(t, el[0])

	// Without signing
	serializedEntity = bytes.NewBuffer(nil)
	err = e.SerializePrivateWithoutSigning(serializedEntity, nil)
	if err != nil {
		t.Fatal(err)
	}
	el, err = ReadKeyRing(serializedEntity)
	if err != nil {
		t.Fatal(err)
	}
	checkV6Key(t, el[0])

	// Private
	serializedEntity = bytes.NewBuffer(nil)
	err = e.SerializePrivate(serializedEntity, nil)
	if err != nil {
		t.Fatal(err)
	}
	el, err = ReadKeyRing(serializedEntity)
	if err != nil {
		t.Fatal(err)
	}
	checkV6Key(t, el[0])
}

func TestNewEntityWithDefaultHashv6(t *testing.T) {
	for _, hash := range hashes[:5] {
		c := &packet.Config{
			V6Keys: true,
			DefaultHash: hash,
		}
		entity, err := NewEntity("Golang Gopher", "Test Key", "no-reply@golang.com", c)
		if hash == crypto.SHA1 {
			if err == nil {
				t.Fatal("should fail on SHA1 key creation")
			}
			continue
		}

		for _, signature := range entity.DirectSignatures {
			prefs := signature.PreferredHash
			if prefs == nil {
				t.Fatal(err)
			}
		}

	}
}
