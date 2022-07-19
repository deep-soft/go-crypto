// Copyright 2011 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package openpgp

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	goerrors "errors"
	"github.com/ProtonMail/go-crypto/openpgp/dilithium_ecdsa"
	"github.com/ProtonMail/go-crypto/openpgp/dilithium_eddsa"
	"io"
	"math/big"
	"time"

	"github.com/ProtonMail/go-crypto/openpgp/ecdh"
	"github.com/ProtonMail/go-crypto/openpgp/ecdsa"
	"github.com/ProtonMail/go-crypto/openpgp/ed25519"
	"github.com/ProtonMail/go-crypto/openpgp/ed448"
	"github.com/ProtonMail/go-crypto/openpgp/eddsa"
	"github.com/ProtonMail/go-crypto/openpgp/errors"
	"github.com/ProtonMail/go-crypto/openpgp/internal/algorithm"
	"github.com/ProtonMail/go-crypto/openpgp/internal/ecc"
	"github.com/ProtonMail/go-crypto/openpgp/kyber_ecdh"
	"github.com/ProtonMail/go-crypto/openpgp/packet"
	"github.com/ProtonMail/go-crypto/openpgp/x25519"
	"github.com/ProtonMail/go-crypto/openpgp/x448"
)

// NewEntity returns an Entity that contains a fresh RSA/RSA keypair with a
// single identity composed of the given full name, comment and email, any of
// which may be empty but must not contain any of "()<>\x00".
// If config is nil, sensible defaults will be used.
func NewEntity(name, comment, email string, config *packet.Config) (*Entity, error) {
	creationTime := config.Now()
	keyLifetimeSecs := config.KeyLifetime()

	// Generate a primary signing key
	primaryPrivRaw, err := newSigner(config)
	if err != nil {
		return nil, err
	}
	primary := packet.NewSignerPrivateKey(creationTime, primaryPrivRaw)
	if config.V6() {
		primary.UpgradeToV6()
	}

	e := &Entity{
		PrimaryKey: &primary.PublicKey,
		PrivateKey: primary,
		Identities: make(map[string]*Identity),
		Subkeys:    []Subkey{},
		Signatures: []*packet.Signature{},
	}

	if config.V6() {
		// In v6 keys algorithm preferences should be stored in direct key signatures
		selfSignature := createSignaturePacket(&primary.PublicKey, packet.SigTypeDirectSignature, config)
		err = writeKeyProperties(selfSignature, creationTime, keyLifetimeSecs, config)
		if err != nil {
			return nil, err
		}
		err = selfSignature.SignDirectKeyBinding(&primary.PublicKey, primary, config)
		if err != nil {
			return nil, err
		}
		e.Signatures = append(e.Signatures, selfSignature)
		e.SelfSignature = selfSignature
	}

	err = e.addUserId(name, comment, email, config, creationTime, keyLifetimeSecs, !config.V6())
	if err != nil {
		return nil, err
	}

	// NOTE: No key expiry here, but we will not return this subkey in EncryptionKey()
	// if the primary/master key has expired.
	err = e.addEncryptionSubkey(config, creationTime, 0)
	if err != nil {
		return nil, err
	}

	return e, nil
}

func (t *Entity) AddUserId(name, comment, email string, config *packet.Config) error {
	creationTime := config.Now()
	keyLifetimeSecs := config.KeyLifetime()
	return t.addUserId(name, comment, email, config, creationTime, keyLifetimeSecs, !config.V6())
}

func writeKeyProperties(selfSignature *packet.Signature, creationTime time.Time, keyLifetimeSecs uint32, config *packet.Config) error {
	selfSignature.CreationTime = creationTime
	selfSignature.KeyLifetimeSecs = &keyLifetimeSecs
	selfSignature.FlagsValid = true
	selfSignature.FlagSign = true
	selfSignature.FlagCertify = true
	selfSignature.SEIPDv1 = true // true by default, see 5.8 vs. 5.14
	selfSignature.SEIPDv2 = config.AEAD() != nil

	// Set the PreferredHash for the SelfSignature from the packet.Config.
	// If it is not the must-implement algorithm from rfc4880bis, append that.
	hash, ok := algorithm.HashToHashId(config.Hash())
	if !ok {
		return errors.UnsupportedError("unsupported preferred hash function")
	}

	selfSignature.PreferredHash = []uint8{hash}
	if config.Hash() != crypto.SHA256 {
		selfSignature.PreferredHash = append(selfSignature.PreferredHash, hashToHashId(crypto.SHA256))
	}

	// Likewise for DefaultCipher.
	selfSignature.PreferredSymmetric = []uint8{uint8(config.Cipher())}
	if config.Cipher() != packet.CipherAES128 {
		selfSignature.PreferredSymmetric = append(selfSignature.PreferredSymmetric, uint8(packet.CipherAES128))
	}

	// We set CompressionNone as the preferred compression algorithm because
	// of compression side channel attacks, then append the configured
	// DefaultCompressionAlgo if any is set (to signal support for cases
	// where the application knows that using compression is safe).
	selfSignature.PreferredCompression = []uint8{uint8(packet.CompressionNone)}
	if config.Compression() != packet.CompressionNone {
		selfSignature.PreferredCompression = append(selfSignature.PreferredCompression, uint8(config.Compression()))
	}

	// And for DefaultMode.
	modes := []uint8{uint8(config.AEAD().Mode())}
	if config.AEAD().Mode() != packet.AEADModeOCB {
		modes = append(modes, uint8(packet.AEADModeOCB))
	}

	// For preferred (AES256, GCM), we'll generate (AES256, GCM), (AES256, OCB), (AES128, GCM), (AES128, OCB)
	for _, cipher := range selfSignature.PreferredSymmetric {
		for _, mode := range modes {
			selfSignature.PreferredCipherSuites = append(selfSignature.PreferredCipherSuites, [2]uint8{cipher, mode})
		}
	}
	return nil
}

func (t *Entity) addUserId(name, comment, email string, config *packet.Config, creationTime time.Time, keyLifetimeSecs uint32, writeProperties bool) error {
	uid := packet.NewUserId(name, comment, email)
	if uid == nil {
		return errors.InvalidArgumentError("user id field contained invalid characters")
	}

	if _, ok := t.Identities[uid.Id]; ok {
		return errors.InvalidArgumentError("user id exist")
	}

	primary := t.PrivateKey
	isPrimaryId := len(t.Identities) == 0
	selfSignature := createSignaturePacket(&primary.PublicKey, packet.SigTypePositiveCert, config)
	if writeProperties {
		err := writeKeyProperties(selfSignature, creationTime, keyLifetimeSecs, config)
		if err != nil {
			return err
		}
	}
	selfSignature.IsPrimaryId = &isPrimaryId

	// User ID binding signature
	err := selfSignature.SignUserId(uid.Id, &primary.PublicKey, primary, config)
	if err != nil {
		return err
	}
	t.Identities[uid.Id] = &Identity{
		Name:          uid.Id,
		UserId:        uid,
		SelfSignature: selfSignature,
		Signatures:    []*packet.Signature{selfSignature},
	}
	return nil
}

// AddSigningSubkey adds a signing keypair as a subkey to the Entity.
// If config is nil, sensible defaults will be used.
func (e *Entity) AddSigningSubkey(config *packet.Config) error {
	creationTime := config.Now()
	keyLifetimeSecs := config.KeyLifetime()

	subPrivRaw, err := newSigner(config)
	if err != nil {
		return err
	}
	sub := packet.NewSignerPrivateKey(creationTime, subPrivRaw)
	sub.IsSubkey = true
	if config.V6() {
		sub.UpgradeToV6()
	}

	subkey := Subkey{
		PublicKey:  &sub.PublicKey,
		PrivateKey: sub,
	}
	subkey.Sig = createSignaturePacket(e.PrimaryKey, packet.SigTypeSubkeyBinding, config)
	subkey.Sig.CreationTime = creationTime
	subkey.Sig.KeyLifetimeSecs = &keyLifetimeSecs
	subkey.Sig.FlagsValid = true
	subkey.Sig.FlagSign = true
	subkey.Sig.EmbeddedSignature = createSignaturePacket(subkey.PublicKey, packet.SigTypePrimaryKeyBinding, config)
	subkey.Sig.EmbeddedSignature.CreationTime = creationTime

	err = subkey.Sig.EmbeddedSignature.CrossSignKey(subkey.PublicKey, e.PrimaryKey, subkey.PrivateKey, config)
	if err != nil {
		return err
	}

	err = subkey.Sig.SignKey(subkey.PublicKey, e.PrivateKey, config)
	if err != nil {
		return err
	}

	e.Subkeys = append(e.Subkeys, subkey)
	return nil
}

// AddEncryptionSubkey adds an encryption keypair as a subkey to the Entity.
// If config is nil, sensible defaults will be used.
func (e *Entity) AddEncryptionSubkey(config *packet.Config) error {
	creationTime := config.Now()
	keyLifetimeSecs := config.KeyLifetime()
	return e.addEncryptionSubkey(config, creationTime, keyLifetimeSecs)
}

func (e *Entity) addEncryptionSubkey(config *packet.Config, creationTime time.Time, keyLifetimeSecs uint32) error {
	subPrivRaw, err := newDecrypter(config)
	if err != nil {
		return err
	}
	sub := packet.NewDecrypterPrivateKey(creationTime, subPrivRaw)
	sub.IsSubkey = true
	if config.V6() {
		sub.UpgradeToV6()
	}

	subkey := Subkey{
		PublicKey:  &sub.PublicKey,
		PrivateKey: sub,
	}
	subkey.Sig = createSignaturePacket(e.PrimaryKey, packet.SigTypeSubkeyBinding, config)
	subkey.Sig.CreationTime = creationTime
	subkey.Sig.KeyLifetimeSecs = &keyLifetimeSecs
	subkey.Sig.FlagsValid = true
	subkey.Sig.FlagEncryptStorage = true
	subkey.Sig.FlagEncryptCommunications = true

	err = subkey.Sig.SignKey(subkey.PublicKey, e.PrivateKey, config)
	if err != nil {
		return err
	}

	e.Subkeys = append(e.Subkeys, subkey)
	return nil
}

// Generates a signing key
func newSigner(config *packet.Config) (signer interface{}, err error) {
	switch config.PublicKeyAlgorithm() {
	case packet.PubKeyAlgoRSA:
		bits := config.RSAModulusBits()
		if bits < 1024 {
			return nil, errors.InvalidArgumentError("bits must be >= 1024")
		}
		if config != nil && len(config.RSAPrimes) >= 2 {
			primes := config.RSAPrimes[0:2]
			config.RSAPrimes = config.RSAPrimes[2:]
			return generateRSAKeyWithPrimes(config.Random(), 2, bits, primes)
		}
		return rsa.GenerateKey(config.Random(), bits)
	case packet.PubKeyAlgoEdDSA:
		if config.V6() {
			// Implementations MUST NOT accept or generate v6 key material
			// using the deprecated OIDs.
			return nil, errors.InvalidArgumentError("EdDSALegacy cannot be used for v6 keys")
		}
		curve := ecc.FindEdDSAByGenName(string(config.CurveName()))
		if curve == nil {
			return nil, errors.InvalidArgumentError("unsupported curve")
		}

		priv, err := eddsa.GenerateKey(config.Random(), curve)
		if err != nil {
			return nil, err
		}
		return priv, nil
	case packet.PubKeyAlgoECDSA:
		curve := ecc.FindECDSAByGenName(string(config.CurveName()))
		if curve == nil {
			return nil, errors.InvalidArgumentError("unsupported curve")
		}

		priv, err := ecdsa.GenerateKey(config.Random(), curve)
		if err != nil {
			return nil, err
		}
		return priv, nil
	case packet.PubKeyAlgoEd25519:
		priv, err := ed25519.GenerateKey(config.Random())
		if err != nil {
			return nil, err
		}
		return priv, nil
	case packet.PubKeyAlgoEd448:
		priv, err := ed448.GenerateKey(config.Random())
		if err != nil {
			return nil, err
		}
		return priv, nil
	case packet.PubKeyAlgoDilithium3p256, packet.PubKeyAlgoDilithium5p384, packet.PubKeyAlgoDilithium3Brainpool256,
		packet.PubKeyAlgoDilithium5Brainpool384:
		if !config.V6() {
			return nil, goerrors.New("openpgp: cannot create a non-v5 dilithium_ecdsa key")
		}

		c, err := packet.GetECDSACurveFromAlgID(config.PublicKeyAlgorithm())
		if err != nil {
			return nil, err
		}
		d, err := packet.GetDilithiumFromAlgID(config.PublicKeyAlgorithm())
		if err != nil {
			return nil, err
		}

		return dilithium_ecdsa.GenerateKey(config.Random(), uint8(config.PublicKeyAlgorithm()), c, d)
	case packet.PubKeyAlgoDilithium3Ed25519, packet.PubKeyAlgoDilithium5Ed448:
		if !config.V6() {
			return nil, goerrors.New("openpgp: cannot create a non-v5 dilithium_eddsa key")
		}

		c, err := packet.GetEdDSACurveFromAlgID(config.PublicKeyAlgorithm())
		if err != nil {
			return nil, err
		}
		d, err := packet.GetDilithiumFromAlgID(config.PublicKeyAlgorithm())
		if err != nil {
			return nil, err
		}

		return dilithium_eddsa.GenerateKey(config.Random(), uint8(config.PublicKeyAlgorithm()), c, d)
	default:
		return nil, errors.InvalidArgumentError("unsupported public key algorithm")
	}
}

// Generates an encryption/decryption key
func newDecrypter(config *packet.Config) (decrypter interface{}, err error) {
	pubKeyAlgo := config.PublicKeyAlgorithm()
	switch pubKeyAlgo {
	case packet.PubKeyAlgoRSA:
		bits := config.RSAModulusBits()
		if bits < 1024 {
			return nil, errors.InvalidArgumentError("bits must be >= 1024")
		}
		if config != nil && len(config.RSAPrimes) >= 2 {
			primes := config.RSAPrimes[0:2]
			config.RSAPrimes = config.RSAPrimes[2:]
			return generateRSAKeyWithPrimes(config.Random(), 2, bits, primes)
		}
		return rsa.GenerateKey(config.Random(), bits)
	case packet.PubKeyAlgoEdDSA, packet.PubKeyAlgoECDSA:
		fallthrough // When passing EdDSA or ECDSA, we generate an ECDH subkey
	case packet.PubKeyAlgoECDH:
		if config.V6() &&
			(config.CurveName() == packet.Curve25519 ||
				config.CurveName() == packet.Curve448) {
			// Implementations MUST NOT accept or generate v6 key material
			// using the deprecated OIDs.
			return nil, errors.InvalidArgumentError("ECDH with Curve25519/448 legacy cannot be used for v6 keys")
		}
		var kdf = ecdh.KDF{
			Hash:   algorithm.SHA512,
			Cipher: algorithm.AES256,
		}
		curve := ecc.FindECDHByGenName(string(config.CurveName()))
		if curve == nil {
			return nil, errors.InvalidArgumentError("unsupported curve")
		}
		return ecdh.GenerateKey(config.Random(), curve, kdf)
	case packet.PubKeyAlgoEd25519, packet.PubKeyAlgoX25519: // When passing Ed25519, we generate an X25519 subkey
		return x25519.GenerateKey(config.Random())
	case packet.PubKeyAlgoEd448, packet.PubKeyAlgoX448: // When passing Ed448, we generate an X448 subkey
		return x448.GenerateKey(config.Random())
	case packet.PubKeyAlgoDilithium3Ed25519, packet.PubKeyAlgoDilithium5Ed448, packet.PubKeyAlgoDilithium3p256,
		packet.PubKeyAlgoDilithium5p384, packet.PubKeyAlgoDilithium3Brainpool256,
		packet.PubKeyAlgoDilithium5Brainpool384:
		if pubKeyAlgo, err = packet.GetMatchingKyberKem(config.PublicKeyAlgorithm()); err != nil {
			return nil, err
		}
		fallthrough // When passing Dilithium + EdDSA or ECDSA, we generate a Kyber + ECDH subkey
	case packet.PubKeyAlgoKyber768X25519, packet.PubKeyAlgoKyber1024X448, packet.PubKeyAlgoKyber768P256,
		packet.PubKeyAlgoKyber1024P384, packet.PubKeyAlgoKyber768Brainpool256, packet.PubKeyAlgoKyber1024Brainpool384:
		if !config.V6() {
			return nil, goerrors.New("openpgp: cannot create a non-v5 kyber_ecdh key")
		}

		c, err := packet.GetECDHCurveFromAlgID(pubKeyAlgo)
		if err != nil {
			return nil, err
		}
		k, err := packet.GetKyberFromAlgID(pubKeyAlgo)
		if err != nil {
			return nil, err
		}

		return kyber_ecdh.GenerateKey(config.Random(), uint8(pubKeyAlgo), c, k)
	default:
		return nil, errors.InvalidArgumentError("unsupported public key algorithm")
	}
}

var bigOne = big.NewInt(1)

// generateRSAKeyWithPrimes generates a multi-prime RSA keypair of the
// given bit size, using the given random source and prepopulated primes.
func generateRSAKeyWithPrimes(random io.Reader, nprimes int, bits int, prepopulatedPrimes []*big.Int) (*rsa.PrivateKey, error) {
	priv := new(rsa.PrivateKey)
	priv.E = 65537

	if nprimes < 2 {
		return nil, goerrors.New("generateRSAKeyWithPrimes: nprimes must be >= 2")
	}

	if bits < 1024 {
		return nil, goerrors.New("generateRSAKeyWithPrimes: bits must be >= 1024")
	}

	primes := make([]*big.Int, nprimes)

NextSetOfPrimes:
	for {
		todo := bits
		// crypto/rand should set the top two bits in each prime.
		// Thus each prime has the form
		//   p_i = 2^bitlen(p_i) × 0.11... (in base 2).
		// And the product is:
		//   P = 2^todo × α
		// where α is the product of nprimes numbers of the form 0.11...
		//
		// If α < 1/2 (which can happen for nprimes > 2), we need to
		// shift todo to compensate for lost bits: the mean value of 0.11...
		// is 7/8, so todo + shift - nprimes * log2(7/8) ~= bits - 1/2
		// will give good results.
		if nprimes >= 7 {
			todo += (nprimes - 2) / 5
		}
		for i := 0; i < nprimes; i++ {
			var err error
			if len(prepopulatedPrimes) == 0 {
				primes[i], err = rand.Prime(random, todo/(nprimes-i))
				if err != nil {
					return nil, err
				}
			} else {
				primes[i] = prepopulatedPrimes[0]
				prepopulatedPrimes = prepopulatedPrimes[1:]
			}

			todo -= primes[i].BitLen()
		}

		// Make sure that primes is pairwise unequal.
		for i, prime := range primes {
			for j := 0; j < i; j++ {
				if prime.Cmp(primes[j]) == 0 {
					continue NextSetOfPrimes
				}
			}
		}

		n := new(big.Int).Set(bigOne)
		totient := new(big.Int).Set(bigOne)
		pminus1 := new(big.Int)
		for _, prime := range primes {
			n.Mul(n, prime)
			pminus1.Sub(prime, bigOne)
			totient.Mul(totient, pminus1)
		}
		if n.BitLen() != bits {
			// This should never happen for nprimes == 2 because
			// crypto/rand should set the top two bits in each prime.
			// For nprimes > 2 we hope it does not happen often.
			continue NextSetOfPrimes
		}

		priv.D = new(big.Int)
		e := big.NewInt(int64(priv.E))
		ok := priv.D.ModInverse(e, totient)

		if ok != nil {
			priv.Primes = primes
			priv.N = n
			break
		}
	}

	priv.Precompute()
	return priv, nil
}
