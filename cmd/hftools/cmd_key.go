package main

import (
	"crypto/ed25519"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/ziozzang/hftools/internal/identity"
	"github.com/ziozzang/hftools/internal/sign"
)

// keyCommand manages the ~/.hftools signing identity: the private key used by
// `hftools sign` and the registry of trusted public keys consulted by
// `hftools verify-sig`.
func keyCommand(args []string) error {
	if len(args) == 0 {
		return keyShow(nil)
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "init":
		return keyInit(rest)
	case "show", "pub", "info":
		return keyShow(rest)
	case "export":
		return keyExport(rest)
	case "trust":
		return keyTrust(rest)
	case "untrust", "revoke":
		return keyUntrust(rest)
	case "list", "trusted", "ls":
		return keyList(rest)
	case "path", "where":
		return keyPath(rest)
	case "help", "-h", "--help":
		keyUsage(os.Stdout)
		return nil
	default:
		keyUsage(os.Stderr)
		return fmt.Errorf("unknown key subcommand %q", sub)
	}
}

func keyUsage(w *os.File) {
	fmt.Fprintln(w, `hftools key - manage the ~/.hftools signing identity

Usage:
  hftools key init [--signer LABEL] [--force]   Create the signing keypair
  hftools key show                              Print this machine's public key and fingerprint
  hftools key export [--out FILE]               Write the public key (PEM) to distribute
  hftools key trust <name> <pubkey|file>        Trust a signer's public key by name
  hftools key untrust <name>                    Remove a trusted key
  hftools key list                              List trusted keys
  hftools key path                              Show home, config, and key paths

Keys live under ~/.hftools (override with HFTOOLS_HOME). Distribute the public
key out-of-band; recipients run "hftools key trust <name> <pubkey>" once, then
"hftools verify-sig" recognizes your signatures automatically.`)
}

func keyInit(args []string) error {
	fs := flag.NewFlagSet("key init", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	signer := fs.String("signer", "", "signer label recorded in signatures (e.g. an email)")
	force := fs.Bool("force", false, "overwrite an existing identity key")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, _, err := identity.LoadConfig()
	if err != nil {
		return err
	}
	keyPath, err := identity.DefaultKeyPath(cfg)
	if err != nil {
		return err
	}
	if _, err := os.Stat(keyPath); err == nil && !*force {
		return fmt.Errorf("identity key already exists at %s\nuse --force to replace it, or run `hftools key show`", keyPath)
	} else if err != nil && !os.IsNotExist(err) {
		return err
	}
	pub, priv, err := sign.GenerateKey()
	if err != nil {
		return err
	}
	privPEM, err := sign.MarshalPrivateKeyPEM(priv)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(keyPath), 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(keyPath, privPEM, 0o600); err != nil {
		return err
	}
	if err := identity.WritePublicKey(pub); err != nil {
		return err
	}
	if *signer != "" {
		cfg.Signer = *signer
	}
	if err := cfg.Save(); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "created signing identity\n  private key: %s (keep secret)\n", keyPath)
	if cfg.Signer != "" {
		fmt.Fprintf(os.Stderr, "  signer:      %s\n", cfg.Signer)
	}
	fmt.Fprintf(os.Stderr, "  fingerprint: %s\n", sign.Fingerprint(pub))
	fmt.Fprintf(os.Stderr, "\nshare your public key so others can verify your signatures:\n")
	fmt.Println(sign.PublicKeyHex(pub))
	return nil
}

func keyShow(args []string) error {
	fs := flag.NewFlagSet("key show", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	pemOut := fs.Bool("pem", false, "also print the PKIX PEM public key")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, _, err := identity.LoadConfig()
	if err != nil {
		return err
	}
	keyPath, err := identity.DefaultKeyPath(cfg)
	if err != nil {
		return err
	}
	priv, err := identity.LoadKey(keyPath)
	if os.IsNotExist(err) {
		return fmt.Errorf("no signing identity yet — run `hftools key init` (or just `hftools sign`, which creates one)")
	}
	if err != nil {
		return err
	}
	pub := priv.Public().(ed25519.PublicKey)
	if cfg.Signer != "" {
		fmt.Fprintf(os.Stderr, "signer:      %s\n", cfg.Signer)
	}
	fmt.Fprintf(os.Stderr, "key:         %s\n", keyPath)
	fmt.Fprintf(os.Stderr, "fingerprint: %s\n", sign.Fingerprint(pub))
	fmt.Println(sign.PublicKeyHex(pub))
	if *pemOut {
		pemBytes, err := sign.MarshalPublicKeyPEM(pub)
		if err != nil {
			return err
		}
		os.Stdout.Write(pemBytes)
	}
	return nil
}

func keyExport(args []string) error {
	fs := flag.NewFlagSet("key export", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	out := fs.String("out", "", "write the PEM public key to this file (default: stdout)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, _, err := identity.LoadConfig()
	if err != nil {
		return err
	}
	keyPath, err := identity.DefaultKeyPath(cfg)
	if err != nil {
		return err
	}
	priv, err := identity.LoadKey(keyPath)
	if os.IsNotExist(err) {
		return fmt.Errorf("no signing identity yet — run `hftools key init`")
	}
	if err != nil {
		return err
	}
	pub := priv.Public().(ed25519.PublicKey)
	pemBytes, err := sign.MarshalPublicKeyPEM(pub)
	if err != nil {
		return err
	}
	if *out == "" {
		os.Stdout.Write(pemBytes)
		return nil
	}
	if err := os.WriteFile(*out, pemBytes, 0o644); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "wrote public key -> %s (fingerprint %s)\n", *out, sign.Fingerprint(pub))
	return nil
}

func keyTrust(args []string) error {
	if len(args) < 2 {
		return fmt.Errorf("usage: hftools key trust <name> <pubkey-hex|pubkey-file|PEM>")
	}
	name, spec := args[0], args[1]
	pub, err := parsePublicKeySpec(spec)
	if err != nil {
		return err
	}
	cfg, _, err := identity.LoadConfig()
	if err != nil {
		return err
	}
	cfg.Trust(name, pub)
	if err := cfg.Save(); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "trusted %q\n  fingerprint: %s\n", name, sign.Fingerprint(pub))
	return nil
}

func keyUntrust(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: hftools key untrust <name>")
	}
	name := args[0]
	cfg, _, err := identity.LoadConfig()
	if err != nil {
		return err
	}
	if _, ok := cfg.TrustedKeys[name]; !ok {
		return fmt.Errorf("no trusted key named %q", name)
	}
	delete(cfg.TrustedKeys, name)
	if err := cfg.Save(); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "removed trusted key %q\n", name)
	return nil
}

func keyList(args []string) error {
	cfg, _, err := identity.LoadConfig()
	if err != nil {
		return err
	}
	names := cfg.TrustedNames()
	if len(names) == 0 {
		fmt.Fprintln(os.Stderr, "no trusted keys — add one with: hftools key trust <name> <pubkey>")
		return nil
	}
	for _, name := range names {
		pub, err := sign.ParsePublicKey(cfg.TrustedKeys[name])
		if err != nil {
			fmt.Printf("%-20s (invalid: %v)\n", name, err)
			continue
		}
		fmt.Printf("%-20s %s  %s\n", name, sign.ShortFingerprint(pub), sign.PublicKeyHex(pub))
	}
	return nil
}

func keyPath(args []string) error {
	dir, err := identity.Dir()
	if err != nil {
		return err
	}
	cfg, cfgPath, err := identity.LoadConfig()
	if err != nil {
		return err
	}
	keyPath, err := identity.DefaultKeyPath(cfg)
	if err != nil {
		return err
	}
	fmt.Printf("home:   %s\n", dir)
	fmt.Printf("config: %s\n", cfgPath)
	fmt.Printf("key:    %s\n", keyPath)
	return nil
}

// parsePublicKeySpec accepts a file path, a hex key, or an inline PEM block.
func parsePublicKeySpec(spec string) (ed25519.PublicKey, error) {
	if b, err := os.ReadFile(spec); err == nil {
		return sign.ParsePublicKey(string(b))
	}
	return sign.ParsePublicKey(spec)
}
