package main

import (
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"dumpcat/internal/update"
)

// updatePublicKeys are the release signing public keys baked in at build time
// (-ldflags "-X main.updatePublicKeys=BASE64[,BASE64]"); DUMPCAT_UPDATE_PUBLIC_KEY adds more.
// With at least one key, only signed releases are installed.
var updatePublicKeys = ""

// trustedUpdateKeys returns the keys releases must be signed with; none means
// signatures are not checked. A malformed key is an error, never ignored.
func trustedUpdateKeys() ([]ed25519.PublicKey, error) {
	if _, err := update.ParsePublicKeys(updatePublicKeys); err != nil {
		return nil, fmt.Errorf("release public key built into this binary: %w", err)
	}
	env := os.Getenv("DUMPCAT_UPDATE_PUBLIC_KEY")
	if _, err := update.ParsePublicKeys(env); err != nil {
		return nil, fmt.Errorf("DUMPCAT_UPDATE_PUBLIC_KEY: %w", err)
	}
	return update.ParsePublicKeys(updatePublicKeys, env)
}

const releaseUsage = `Usage:
  dumpcat release keygen [-out DIR] [-name NAME]   create a signing key pair (NAME.key private, NAME.pub public)
  dumpcat release sign -key FILE [-key FILE] [-out SIG] latest.json
                                                   write latest.json.sig
  dumpcat release verify [-pub KEYS|FILE] [-sig SIG] latest.json
                                                   check a signature (default: the keys this binary trusts)
  dumpcat release pubkey [-key FILE] [KEY ...]     print the public key(s), comma-separated, for -X main.updatePublicKeys`

// releaseCmd: tools for publishing signed releases.
func releaseCmd(args []string) error {
	if len(args) == 0 {
		return errors.New("missing subcommand\n" + releaseUsage)
	}
	switch args[0] {
	case "keygen":
		return releaseKeygen(args[1:])
	case "sign":
		return releaseSign(args[1:])
	case "verify":
		return releaseVerify(args[1:])
	case "pubkey":
		return releasePubkey(args[1:])
	case "help", "-h", "--help":
		fmt.Println(releaseUsage)
		return nil
	}
	return fmt.Errorf("unknown subcommand %q\n%s", args[0], releaseUsage)
}

func releaseKeygen(args []string) error {
	fs := flag.NewFlagSet("release keygen", flag.ExitOnError)
	out := fs.String("out", ".", "directory for the key files")
	name := fs.String("name", "dumpcat-release", "file name of the key pair, without extension")
	_ = fs.Parse(args)
	keyFile, pubFile, pub, err := keygen(*out, *name)
	if err != nil {
		return err
	}
	fmt.Printf(`Private signing key: %s
  Keep it secret and offline — whoever has it can publish updates that every installation trusts.
  Never copy it to the release server. Keep an offline backup: without it you cannot sign new releases.
Public key:          %s  (%s)

Build signed releases with:
  UPDATE_SIGNING_KEY=%s ./build.sh
  (Windows: $env:UPDATE_SIGNING_KEY = '%s'; .\build.ps1)
`, keyFile, update.EncodePublicKey(pub), pubFile, keyFile, keyFile)
	return nil
}

// keygen writes NAME.key (0600) and NAME.pub in dir; existing files are never overwritten.
func keygen(dir, name string) (keyFile, pubFile string, pub ed25519.PublicKey, err error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", "", nil, err
	}
	keyFile, pubFile = filepath.Join(dir, name+".key"), filepath.Join(dir, name+".pub")
	for _, f := range []string{keyFile, pubFile} {
		if _, err := os.Stat(f); err == nil {
			return "", "", nil, fmt.Errorf("%s already exists — not overwriting a key", f)
		}
	}
	pub, priv, err := update.GenerateKey()
	if err != nil {
		return "", "", nil, err
	}
	f, err := os.OpenFile(keyFile, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", "", nil, err
	}
	_, err = f.Write(update.EncodePrivateKey(priv))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.WriteFile(pubFile, []byte(update.EncodePublicKey(pub)+"\n"), 0o644)
	}
	if err != nil {
		os.Remove(keyFile)
		os.Remove(pubFile)
		return "", "", nil, err
	}
	return keyFile, pubFile, pub, nil
}

type multiFlag []string

func (m *multiFlag) String() string     { return strings.Join(*m, ",") }
func (m *multiFlag) Set(v string) error { *m = append(*m, v); return nil }

func releaseSign(args []string) error {
	fs := flag.NewFlagSet("release sign", flag.ExitOnError)
	var keys multiFlag
	fs.Var(&keys, "key", "private signing key file (repeat to sign with several keys, e.g. during a key rotation)")
	out := fs.String("out", "", "signature file (default: <file>.sig)")
	_ = fs.Parse(args)
	if fs.NArg() != 1 || len(keys) == 0 {
		return errors.New("usage: dumpcat release sign -key FILE [-key FILE] [-out SIG] latest.json")
	}
	file := fs.Arg(0)
	sigFile := *out
	if sigFile == "" {
		sigFile = file + update.SignatureSuffix
	}
	pubs, err := signManifest(file, sigFile, keys)
	if err != nil {
		return err
	}
	fmt.Printf("Signed %s → %s\n", file, sigFile)
	for _, p := range pubs {
		fmt.Printf("  key %s\n", update.EncodePublicKey(p))
	}
	fmt.Println("Upload it together with latest.json; do not change latest.json after signing.")
	return nil
}

// signManifest signs the exact bytes of file with every key and writes the
// signatures (one per line) to sigFile.
func signManifest(file, sigFile string, keyFiles []string) ([]ed25519.PublicKey, error) {
	manifest, err := os.ReadFile(file)
	if err != nil {
		return nil, err
	}
	if !json.Valid(manifest) {
		return nil, fmt.Errorf("%s is not a JSON file", file)
	}
	var sig []byte
	var pubs []ed25519.PublicKey
	for _, kf := range keyFiles {
		data, err := os.ReadFile(kf)
		if err != nil {
			return nil, err
		}
		priv, err := update.ParsePrivateKey(data)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", kf, err)
		}
		sig = append(sig, update.Sign(priv, manifest)...)
		pubs = append(pubs, priv.Public().(ed25519.PublicKey))
	}
	for _, p := range pubs {
		if _, err := update.Verify(manifest, sig, []ed25519.PublicKey{p}); err != nil {
			return nil, fmt.Errorf("self-check of the signature failed: %w", err)
		}
	}
	tmp := sigFile + ".tmp"
	if err := os.WriteFile(tmp, sig, 0o644); err != nil {
		return nil, err
	}
	if err := os.Rename(tmp, sigFile); err != nil {
		os.Remove(tmp)
		return nil, err
	}
	return pubs, nil
}

func releaseVerify(args []string) error {
	fs := flag.NewFlagSet("release verify", flag.ExitOnError)
	pubArg := fs.String("pub", "", "trusted public key(s), base64 and comma-separated, or a .pub file (default: the keys this binary trusts)")
	sigArg := fs.String("sig", "", "signature file (default: <file>.sig)")
	_ = fs.Parse(args)
	if fs.NArg() != 1 {
		return errors.New("usage: dumpcat release verify [-pub KEYS|FILE] [-sig SIG] latest.json")
	}
	file := fs.Arg(0)
	sigFile := *sigArg
	if sigFile == "" {
		sigFile = file + update.SignatureSuffix
	}
	var keys []ed25519.PublicKey
	var err error
	if *pubArg != "" {
		list := *pubArg
		if b, rerr := os.ReadFile(list); rerr == nil {
			list = string(b)
		}
		keys, err = update.ParsePublicKeys(list)
	} else {
		keys, err = trustedUpdateKeys()
	}
	if err != nil {
		return err
	}
	if len(keys) == 0 {
		return errors.New("no public key: pass -pub KEY (this binary has no release key built in)")
	}
	manifest, err := os.ReadFile(file)
	if err != nil {
		return err
	}
	sig, err := os.ReadFile(sigFile)
	if err != nil {
		return err
	}
	k, err := update.Verify(manifest, sig, keys)
	if err != nil {
		return err
	}
	fmt.Printf("OK: %s is signed by %s\n", file, update.EncodePublicKey(k))
	return nil
}

func releasePubkey(args []string) error {
	fs := flag.NewFlagSet("release pubkey", flag.ExitOnError)
	keyFile := fs.String("key", "", "private signing key file")
	_ = fs.Parse(args)
	var list []string
	if *keyFile != "" {
		data, err := os.ReadFile(*keyFile)
		if err != nil {
			return err
		}
		priv, err := update.ParsePrivateKey(data)
		if err != nil {
			return fmt.Errorf("%s: %w", *keyFile, err)
		}
		list = append(list, update.EncodePublicKey(priv.Public().(ed25519.PublicKey)))
	}
	keys, err := update.ParsePublicKeys(append(list, fs.Args()...)...)
	if err != nil {
		return err
	}
	if len(keys) == 0 {
		return errors.New("usage: dumpcat release pubkey [-key FILE] [KEY ...]")
	}
	out := make([]string, len(keys))
	for i, k := range keys {
		out[i] = update.EncodePublicKey(k)
	}
	fmt.Println(strings.Join(out, ","))
	return nil
}
