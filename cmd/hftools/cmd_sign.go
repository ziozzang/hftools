package main

import (
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/ziozzang/hftools/internal/identity"
	"github.com/ziozzang/hftools/internal/sign"
	"github.com/ziozzang/hftools/internal/state"
)

const signatureFile = ".sha256.sig"

// signCommand signs a repository's content-addressed .sha256 manifest so a
// recipient can prove provenance, not just integrity.
func signCommand(args []string) error {
	fs := flag.NewFlagSet("sign", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	output := fs.String("output", ".", "downloaded repository directory")
	keyPath := fs.String("key", "", "ed25519 private key PEM to sign with (default: ~/.hftools identity)")
	genKey := fs.String("gen-key", "", "generate a new private key, write it to this path, and sign with it")
	signer := fs.String("signer", "", "optional signer label recorded in the signature (default: config.yaml signer)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	root, err := resolveDir(*output)
	if err != nil {
		return err
	}
	stateDir, err := stateDirectory(root)
	if err != nil {
		return err
	}
	if _, err := os.ReadFile(filepath.Join(root, ".sha256")); err != nil {
		return fmt.Errorf("no .sha256 manifest in %s (run download or verify first): %w", root, err)
	}

	var priv ed25519.PrivateKey
	switch {
	case *genKey != "":
		if _, err := os.Stat(*genKey); err == nil {
			return fmt.Errorf("refusing to overwrite existing key file %s", *genKey)
		}
		var pub ed25519.PublicKey
		pub, priv, err = sign.GenerateKey()
		if err != nil {
			return err
		}
		pemBytes, err := sign.MarshalPrivateKeyPEM(priv)
		if err != nil {
			return err
		}
		if err := os.WriteFile(*genKey, pemBytes, 0o600); err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "generated ed25519 key -> %s\n", *genKey)
		fmt.Fprintf(os.Stderr, "public key: %s\n", sign.PublicKeyHex(pub))
	case *keyPath != "":
		b, err := os.ReadFile(*keyPath)
		if err != nil {
			return err
		}
		priv, err = sign.ParsePrivateKeyPEM(b)
		if err != nil {
			return err
		}
	default:
		// No explicit key: use (and create on first run) the ~/.hftools identity.
		cfg, _, err := identity.LoadConfig()
		if err != nil {
			return err
		}
		var created bool
		var idKeyPath string
		priv, idKeyPath, created, err = identity.EnsureKey(cfg)
		if err != nil {
			return err
		}
		if created {
			// Materialize ~/.hftools/config.yaml on first use, recording the
			// signer label so later signs default to it.
			if *signer != "" {
				cfg.Signer = *signer
			}
			if err := cfg.Save(); err != nil {
				return err
			}
			pub := priv.Public().(ed25519.PublicKey)
			fmt.Fprintf(os.Stderr, "created signing identity at %s\npublic key: %s\nfingerprint: %s\n",
				idKeyPath, sign.PublicKeyHex(pub), sign.Fingerprint(pub))
		}
		if *signer == "" {
			*signer = cfg.Signer
		}
	}

	rec, err := signManifest(root, stateDir, priv, *signer)
	if err != nil {
		return err
	}
	pub := priv.Public().(ed25519.PublicKey)
	fmt.Printf("signed %s\npublic key: %s\nfingerprint: %s\n", root, rec.PublicKey, sign.Fingerprint(pub))
	fmt.Printf("distribute the public key out-of-band; verify with: hftools verify-sig --output <dir> --pubkey %s\n", rec.PublicKey)
	return nil
}

// signManifest signs root/.sha256 with priv and persists the detached signature
// to stateDir/signature.json and a portable copy at root/.sha256.sig so it
// travels with a flat directory copied across an air gap.
func signManifest(root, stateDir string, priv ed25519.PrivateKey, signer string) (sign.Record, error) {
	payload, err := os.ReadFile(filepath.Join(root, ".sha256"))
	if err != nil {
		return sign.Record{}, fmt.Errorf("no .sha256 manifest in %s (run download or verify first): %w", root, err)
	}
	rec := sign.Sign(payload, priv, signer, time.Now().UTC())
	if err := state.SaveJSONAtomic(filepath.Join(stateDir, "signature.json"), rec); err != nil {
		return rec, err
	}
	if err := state.SaveJSONAtomic(filepath.Join(root, signatureFile), rec); err != nil {
		return rec, err
	}
	return rec, nil
}

// homeAutoSign reports whether config.yaml opts into signing every download and
// verify by default. Errors resolve to false so a missing/broken config never
// blocks a transfer.
func homeAutoSign() bool {
	cfg, _, err := identity.LoadConfig()
	if err != nil {
		return false
	}
	return cfg.AutoSign
}

// autoSignRepo signs a freshly downloaded or verified repository with the
// ~/.hftools identity, creating that identity on first use. It is the hook
// behind --sign and config.yaml auto_sign.
func autoSignRepo(root, stateDir string) error {
	cfg, _, err := identity.LoadConfig()
	if err != nil {
		return err
	}
	priv, keyPath, created, err := identity.EnsureKey(cfg)
	if err != nil {
		return err
	}
	pub := priv.Public().(ed25519.PublicKey)
	if created {
		if err := cfg.Save(); err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "created signing identity at %s (fingerprint %s)\n", keyPath, sign.Fingerprint(pub))
	}
	if _, err := signManifest(root, stateDir, priv, cfg.Signer); err != nil {
		return err
	}
	if cfg.Signer == "" {
		// Without a label the signature proves a key held these bytes but not
		// who that key belongs to, which defeats the point of an audit trail.
		fmt.Fprintf(os.Stderr, "signed %s (fingerprint %s, no signer label — set one with 'hftools key init --signer you@example.com')\n", root, sign.Fingerprint(pub))
		return nil
	}
	fmt.Fprintf(os.Stderr, "signed %s as %s (fingerprint %s)\n", root, cfg.Signer, sign.Fingerprint(pub))
	return nil
}

func loadSignature(root, stateDir string) (sign.Record, error) {
	var rec sign.Record
	for _, p := range []string{filepath.Join(stateDir, "signature.json"), filepath.Join(root, signatureFile)} {
		b, err := os.ReadFile(p)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return rec, err
		}
		if err := json.Unmarshal(b, &rec); err != nil {
			return rec, fmt.Errorf("signature parse: %w", err)
		}
		return rec, nil
	}
	return rec, fmt.Errorf("no signature found (run hftools sign first)")
}

func verifySigCommand(args []string) error {
	fs := flag.NewFlagSet("verify-sig", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	output := fs.String("output", ".", "downloaded repository directory")
	pubkey := fs.String("pubkey", "", "pinned public key to prove provenance: a trusted name, hex, PEM, or file path")
	requireIdentity := fs.Bool("require-signed-identity", false, "fail if the signer label and time are not covered by the signature (pre-v2 records)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	root, err := resolveDir(*output)
	if err != nil {
		return err
	}
	stateDir, err := stateDirectory(root)
	if err != nil {
		return err
	}
	cfg, _, err := identity.LoadConfig()
	if err != nil {
		return err
	}
	res, err := verifyRepoSignature(root, stateDir, cfg, *pubkey)
	if err != nil {
		return err
	}
	if err := checkSignedIdentity(res.Record, cfg, *requireIdentity); err != nil {
		return err
	}
	fmt.Printf("signature OK\n%s\n", signerLine(res.Record))
	fmt.Printf("public key: %s\nfingerprint: %s\n", res.Record.PublicKey, sign.Fingerprint(res.PublicKey))
	if res.TrustLabel != "" {
		fmt.Printf("trusted key: %s\n", res.TrustLabel)
	}
	if !res.Pinned {
		fmt.Fprintln(os.Stderr, "note: key not pinned or trusted — this proves the content is unchanged since signing, not who signed it")
		fmt.Fprintf(os.Stderr, "to trust this signer for future verifications: hftools key trust <name> %s\n", res.Record.PublicKey)
	}
	return nil
}

// signerLine renders who signed a repository and when, so every verification
// leaves an auditable trace of who is answerable for the content.
//
// When the signature does not cover these fields (schema v1) the warning comes
// first and the label is quoted, because such a label proves nothing: anyone can
// rewrite a v2 record as v1 with a forged signer and it still verifies. Use
// --require-signed-identity to reject those records outright.
func signerLine(rec sign.Record) string {
	who := rec.Signer
	if who == "" {
		who = "(no signer label recorded)"
	}
	when := rec.SignedAt.UTC().Format(time.RFC3339)
	if !rec.MetadataSigned() {
		return fmt.Sprintf("UNVERIFIED signer %q, time %s — not covered by the signature (pre-v2 record); treat as unproven", who, when)
	}
	return fmt.Sprintf("signed by %s at %s", who, when)
}

// requireSignedIdentity reports whether a record's identity must be rejected,
// honoring the --require-signed-identity flag and config.yaml.
func identityEnforced(cfg *identity.Config, flagSet bool) bool {
	if flagSet {
		return true
	}
	return cfg != nil && cfg.RequireSignedIdentity
}

// checkSignedIdentity fails a verification whose signer/time are unauthenticated
// when the operator has asked for attribution to be enforced.
func checkSignedIdentity(rec sign.Record, cfg *identity.Config, flagSet bool) error {
	if !identityEnforced(cfg, flagSet) || rec.MetadataSigned() {
		return nil
	}
	return fmt.Errorf("signer identity is not covered by the signature (pre-v2 record); re-sign with this version or drop --require-signed-identity")
}

// sigResult carries the outcome of verifying a repository's stored signature.
type sigResult struct {
	Record     sign.Record
	PublicKey  ed25519.PublicKey
	TrustLabel string // trusted name when the signing key is recognized
	Pinned     bool   // true when provenance (not just integrity) was proven
}

// verifyRepoSignature verifies the signature stored for the repository at root
// against its .sha256 manifest. pubkeySpec (optional) pins a key by trusted name,
// hex, PEM, or file; otherwise a key already in trusted_keys is recognized
// automatically. It is shared by `verify-sig` and `verify`/`verify-batch --verify-sig`.
func verifyRepoSignature(root, stateDir string, cfg *identity.Config, pubkeySpec string) (sigResult, error) {
	payload, err := os.ReadFile(filepath.Join(root, ".sha256"))
	if err != nil {
		return sigResult{}, fmt.Errorf("no .sha256 manifest in %s: %w", root, err)
	}
	rec, err := loadSignature(root, stateDir)
	if err != nil {
		return sigResult{}, err
	}
	recPub, err := sign.ParsePublicKey(rec.PublicKey)
	if err != nil {
		return sigResult{}, fmt.Errorf("signature public key: %w", err)
	}
	var pinned ed25519.PublicKey
	var trustLabel string
	if pubkeySpec != "" {
		if pinned, trustLabel, err = cfg.ResolvePinned(pubkeySpec); err != nil {
			return sigResult{}, err
		}
	} else if name := cfg.LookupTrustedName(recPub); name != "" {
		pinned, trustLabel = recPub, name
	}
	if err := rec.Verify(payload, pinned); err != nil {
		return sigResult{}, err
	}
	return sigResult{Record: rec, PublicKey: recPub, TrustLabel: trustLabel, Pinned: pinned != nil}, nil
}
