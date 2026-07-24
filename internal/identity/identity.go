// Package identity manages hftools' per-user signing identity under ~/.hftools:
// a persistent ed25519 private key, a config.yaml holding the signer label and a
// registry of trusted public keys, and helpers to resolve a pinned key by name,
// file, hex, or PEM. It lets `hftools sign` use a stable home key by default and
// lets `hftools verify-sig` recognize a signer the operator has chosen to trust.
package identity

import (
	"crypto/ed25519"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/ziozzang/hftools/internal/sign"
)

// Filenames inside the home directory.
const (
	ConfigName  = "config.yaml"
	KeyName     = "signing.key" // PKCS#8 PEM ed25519 private key, 0600
	PubName     = "signing.pub" // PKIX PEM public key, distributable
	dirModePerm = 0o700
)

// Config is the parsed ~/.hftools/config.yaml. Unknown keys are ignored so newer
// fields degrade gracefully on older binaries.
type Config struct {
	Signer   string // label recorded in signatures (e.g. an email)
	KeyPath  string // private key path; ~ is expanded, blank means the default
	AutoSign bool   // sign every download/verify with the home identity by default
	// RequireSignedIdentity rejects signatures whose signer label and time are
	// not covered by the signature (schema v1). Without it such a record still
	// verifies, and its label is only displayed as unverified — which an
	// attacker can exploit by rewriting a v2 record as v1 with a forged label.
	// Turn it on wherever the signer identity is relied on for attribution.
	RequireSignedIdentity bool
	TrustedKeys           map[string]string // name -> hex-encoded ed25519 public key
}

// Dir returns the hftools home directory. HFTOOLS_HOME overrides it (used by
// tests and for isolated setups); otherwise it is ~/.hftools.
func Dir() (string, error) {
	if v := strings.TrimSpace(os.Getenv("HFTOOLS_HOME")); v != "" {
		return v, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".hftools"), nil
}

// ConfigPath returns the config.yaml path within the home directory.
func ConfigPath() (string, error) {
	dir, err := Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, ConfigName), nil
}

// DefaultKeyPath returns the path a config's key resolves to: an explicit,
// tilde-expanded KeyPath, or <home>/signing.key.
func DefaultKeyPath(cfg *Config) (string, error) {
	dir, err := Dir()
	if err != nil {
		return "", err
	}
	if cfg != nil && strings.TrimSpace(cfg.KeyPath) != "" {
		return expandPath(cfg.KeyPath, dir)
	}
	return filepath.Join(dir, KeyName), nil
}

// expandPath resolves ~ and ~/ against the user's home and makes relative paths
// relative to the hftools home directory.
func expandPath(p, homeDir string) (string, error) {
	p = strings.TrimSpace(p)
	if p == "~" || strings.HasPrefix(p, "~/") {
		uh, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		if p == "~" {
			return uh, nil
		}
		return filepath.Join(uh, p[2:]), nil
	}
	if filepath.IsAbs(p) {
		return p, nil
	}
	return filepath.Join(homeDir, p), nil
}

// LoadConfig reads config.yaml, returning zero-valued defaults (not an error)
// when the file does not exist yet. The second return is the resolved path.
func LoadConfig() (*Config, string, error) {
	path, err := ConfigPath()
	if err != nil {
		return nil, "", err
	}
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return &Config{TrustedKeys: map[string]string{}}, path, nil
	}
	if err != nil {
		return nil, path, err
	}
	cfg, err := parseYAML(b)
	if err != nil {
		return nil, path, fmt.Errorf("%s: %w", path, err)
	}
	return cfg, path, nil
}

// Save writes cfg to config.yaml (0600), creating the home directory as needed.
func (c *Config) Save() error {
	path, err := ConfigPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), dirModePerm); err != nil {
		return err
	}
	return os.WriteFile(path, marshalYAML(c), 0o600)
}

// LoadKey reads and parses a PKCS#8 PEM ed25519 private key from path.
func LoadKey(path string) (ed25519.PrivateKey, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return sign.ParsePrivateKeyPEM(b)
}

// EnsureKey returns the home signing key, generating and persisting a fresh
// keypair (private 0600, public PEM alongside) on first use. The bool reports
// whether a new key was created.
func EnsureKey(cfg *Config) (ed25519.PrivateKey, string, bool, error) {
	keyPath, err := DefaultKeyPath(cfg)
	if err != nil {
		return nil, "", false, err
	}
	if priv, err := LoadKey(keyPath); err == nil {
		return priv, keyPath, false, nil
	} else if !os.IsNotExist(err) {
		return nil, keyPath, false, err
	}
	pub, priv, err := sign.GenerateKey()
	if err != nil {
		return nil, keyPath, false, err
	}
	if err := os.MkdirAll(filepath.Dir(keyPath), dirModePerm); err != nil {
		return nil, keyPath, false, err
	}
	privPEM, err := sign.MarshalPrivateKeyPEM(priv)
	if err != nil {
		return nil, keyPath, false, err
	}
	if err := os.WriteFile(keyPath, privPEM, 0o600); err != nil {
		return nil, keyPath, false, err
	}
	if err := WritePublicKey(pub); err != nil {
		return nil, keyPath, false, err
	}
	return priv, keyPath, true, nil
}

// WritePublicKey writes the distributable PKIX PEM public key to <home>/signing.pub.
func WritePublicKey(pub ed25519.PublicKey) error {
	dir, err := Dir()
	if err != nil {
		return err
	}
	pemBytes, err := sign.MarshalPublicKeyPEM(pub)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, dirModePerm); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, PubName), pemBytes, 0o644)
}

// Trust records name -> public key (stored as hex) in cfg, replacing any prior
// entry for that name. It returns the parsed key so callers can echo details.
func (c *Config) Trust(name string, pub ed25519.PublicKey) {
	if c.TrustedKeys == nil {
		c.TrustedKeys = map[string]string{}
	}
	c.TrustedKeys[name] = sign.PublicKeyHex(pub)
}

// TrustedNames returns the trusted key names in sorted order.
func (c *Config) TrustedNames() []string {
	names := make([]string, 0, len(c.TrustedKeys))
	for name := range c.TrustedKeys {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// LookupTrustedName returns the trusted name registered for pub, or "" if the
// key is not trusted.
func (c *Config) LookupTrustedName(pub ed25519.PublicKey) string {
	want := sign.PublicKeyHex(pub)
	for _, name := range c.TrustedNames() {
		if strings.EqualFold(c.TrustedKeys[name], want) {
			return name
		}
	}
	return ""
}

// ResolvePinned turns a --pubkey spec into a pinned key. spec may be a trusted
// key name from cfg, a file path, a hex key, or an inline PEM block. The returned
// label describes what matched (for display); it is empty for anonymous keys.
func (c *Config) ResolvePinned(spec string) (ed25519.PublicKey, string, error) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return nil, "", nil
	}
	// A bare trusted name wins so `verify-sig --pubkey alice` is ergonomic.
	if c != nil {
		if hexKey, ok := c.TrustedKeys[spec]; ok {
			pub, err := sign.ParsePublicKey(hexKey)
			if err != nil {
				return nil, "", fmt.Errorf("trusted key %q: %w", spec, err)
			}
			return pub, spec, nil
		}
	}
	if b, err := os.ReadFile(spec); err == nil {
		pub, err := sign.ParsePublicKey(string(b))
		if err != nil {
			return nil, "", fmt.Errorf("%s: %w", spec, err)
		}
		return pub, "", nil
	}
	pub, err := sign.ParsePublicKey(spec)
	if err != nil {
		return nil, "", err
	}
	if name := c.LookupTrustedName(pub); name != "" {
		return pub, name, nil
	}
	return pub, "", nil
}
