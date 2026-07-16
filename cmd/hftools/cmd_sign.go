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

	"github.com/ziozzang/hfdownload/internal/sign"
	"github.com/ziozzang/hfdownload/internal/state"
)

const signatureFile = ".sha256.sig"

// signCommand signs a repository's content-addressed .sha256 manifest so a
// recipient can prove provenance, not just integrity.
func signCommand(args []string) error {
	fs := flag.NewFlagSet("sign", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	output := fs.String("output", ".", "downloaded repository directory")
	keyPath := fs.String("key", "", "ed25519 private key PEM to sign with")
	genKey := fs.String("gen-key", "", "generate a new private key, write it to this path, and sign with it")
	signer := fs.String("signer", "", "optional signer label recorded in the signature")
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
	payload, err := os.ReadFile(filepath.Join(root, ".sha256"))
	if err != nil {
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
		return fmt.Errorf("provide --key PRIV.pem or --gen-key OUT.pem")
	}

	rec := sign.Sign(payload, priv, *signer, time.Now().UTC())
	if err := state.SaveJSONAtomic(filepath.Join(stateDir, "signature.json"), rec); err != nil {
		return err
	}
	// Also drop a portable copy beside .sha256 so the signature travels with a
	// flat directory copied across an air gap.
	if err := state.SaveJSONAtomic(filepath.Join(root, signatureFile), rec); err != nil {
		return err
	}
	fmt.Printf("signed %s\npublic key: %s\ndistribute the public key out-of-band; verify with: hftools verify-sig --pubkey %s\n",
		root, rec.PublicKey, rec.PublicKey)
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
	pubkey := fs.String("pubkey", "", "pinned public key (hex, PEM, or a file path) to prove provenance")
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
	payload, err := os.ReadFile(filepath.Join(root, ".sha256"))
	if err != nil {
		return fmt.Errorf("no .sha256 manifest in %s: %w", root, err)
	}
	rec, err := loadSignature(root, stateDir)
	if err != nil {
		return err
	}
	var pinned ed25519.PublicKey
	if *pubkey != "" {
		data := *pubkey
		if b, err := os.ReadFile(*pubkey); err == nil {
			data = string(b)
		}
		pinned, err = sign.ParsePublicKey(data)
		if err != nil {
			return err
		}
	}
	if err := rec.Verify(payload, pinned); err != nil {
		return err
	}
	fmt.Printf("signature OK\nsigned by: %s\n", rec.PublicKey)
	if rec.Signer != "" {
		fmt.Printf("signer: %s\n", rec.Signer)
	}
	fmt.Printf("signed at: %s\n", rec.SignedAt.Format(time.RFC3339))
	if pinned == nil {
		fmt.Fprintln(os.Stderr, "note: no --pubkey pinned; this proves the content is unchanged since signing, not who signed it")
	}
	return nil
}
