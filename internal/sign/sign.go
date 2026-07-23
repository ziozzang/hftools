// Package sign provides ed25519 provenance signatures over a repository's
// content digest. hftools already records a content-addressed SHA-256 manifest
// (.sha256) for every download; signing that file lets an air-gapped recipient
// confirm not only that the bytes are intact (which hashing alone proves) but
// that they came from a holder of a specific private key — a trust chain that
// survives transfer over untrusted media.
package sign

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"strings"
	"time"
)

// RecordVersion is the on-disk signature schema version.
const RecordVersion = 1

// Record is the detached signature stored alongside a repository.
type Record struct {
	Version      int       `json:"version"`
	Algorithm    string    `json:"algorithm"`
	Signer       string    `json:"signer,omitempty"`
	PublicKey    string    `json:"public_key"`    // hex-encoded 32-byte ed25519 public key
	Signature    string    `json:"signature"`     // hex-encoded 64-byte signature
	DigestSHA256 string    `json:"digest_sha256"` // hex sha256 of the signed payload
	SignedAt     time.Time `json:"signed_at"`
}

// GenerateKey returns a fresh ed25519 keypair.
func GenerateKey() (ed25519.PublicKey, ed25519.PrivateKey, error) {
	return ed25519.GenerateKey(rand.Reader)
}

// MarshalPrivateKeyPEM encodes a private key as a PKCS#8 PEM block.
func MarshalPrivateKeyPEM(priv ed25519.PrivateKey) ([]byte, error) {
	der, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), nil
}

// ParsePrivateKeyPEM decodes a PKCS#8 PEM ed25519 private key.
func ParsePrivateKeyPEM(data []byte) (ed25519.PrivateKey, error) {
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, fmt.Errorf("no PEM block found")
	}
	key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, err
	}
	priv, ok := key.(ed25519.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("not an ed25519 private key")
	}
	return priv, nil
}

// PublicKeyHex renders a public key as hex.
func PublicKeyHex(pub ed25519.PublicKey) string { return hex.EncodeToString(pub) }

// MarshalPublicKeyPEM encodes a public key as a PKIX PEM block, the portable
// form recipients pin out-of-band.
func MarshalPublicKeyPEM(pub ed25519.PublicKey) ([]byte, error) {
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		return nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}), nil
}

// Fingerprint is the SHA-256 of the raw 32-byte public key, hex-encoded. It is
// the stable, human-comparable identifier used to pin trust in a signer.
func Fingerprint(pub ed25519.PublicKey) string {
	sum := sha256.Sum256(pub)
	return hex.EncodeToString(sum[:])
}

// ShortFingerprint returns the first 16 hex chars of the fingerprint for compact
// display; the full fingerprint remains the value to compare for trust.
func ShortFingerprint(pub ed25519.PublicKey) string {
	fp := Fingerprint(pub)
	if len(fp) > 16 {
		return fp[:16]
	}
	return fp
}

// ParsePublicKey accepts a hex-encoded key or a PEM/PKIX block.
func ParsePublicKey(s string) (ed25519.PublicKey, error) {
	s = strings.TrimSpace(s)
	if strings.Contains(s, "BEGIN") {
		block, _ := pem.Decode([]byte(s))
		if block == nil {
			return nil, fmt.Errorf("no PEM block found")
		}
		key, err := x509.ParsePKIXPublicKey(block.Bytes)
		if err != nil {
			return nil, err
		}
		pub, ok := key.(ed25519.PublicKey)
		if !ok {
			return nil, fmt.Errorf("not an ed25519 public key")
		}
		return pub, nil
	}
	raw, err := hex.DecodeString(s)
	if err != nil {
		return nil, fmt.Errorf("public key is not valid hex or PEM: %w", err)
	}
	if len(raw) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("ed25519 public key must be %d bytes, got %d", ed25519.PublicKeySize, len(raw))
	}
	return ed25519.PublicKey(raw), nil
}

// Sign produces a Record over message using priv.
func Sign(message []byte, priv ed25519.PrivateKey, signer string, now time.Time) Record {
	sig := ed25519.Sign(priv, message)
	digest := sha256.Sum256(message)
	return Record{
		Version:      RecordVersion,
		Algorithm:    "ed25519",
		Signer:       signer,
		PublicKey:    PublicKeyHex(priv.Public().(ed25519.PublicKey)),
		Signature:    hex.EncodeToString(sig),
		DigestSHA256: hex.EncodeToString(digest[:]),
		SignedAt:     now,
	}
}

// Verify checks the record against message. When pinned is non-nil the record's
// embedded key must equal it (provenance); otherwise only tamper-evidence
// against the embedded key is proven, which the caller should flag.
func (r Record) Verify(message []byte, pinned ed25519.PublicKey) error {
	if r.Algorithm != "ed25519" {
		return fmt.Errorf("unsupported signature algorithm %q", r.Algorithm)
	}
	pub, err := ParsePublicKey(r.PublicKey)
	if err != nil {
		return fmt.Errorf("record public key: %w", err)
	}
	if pinned != nil && !pub.Equal(pinned) {
		return fmt.Errorf("signature key %s does not match the pinned key", r.PublicKey)
	}
	sig, err := hex.DecodeString(r.Signature)
	if err != nil {
		return fmt.Errorf("signature is not valid hex: %w", err)
	}
	digest := sha256.Sum256(message)
	if hex.EncodeToString(digest[:]) != r.DigestSHA256 {
		return fmt.Errorf("signed payload digest mismatch: content changed since signing")
	}
	if !ed25519.Verify(pub, message, sig) {
		return fmt.Errorf("signature verification failed")
	}
	return nil
}
