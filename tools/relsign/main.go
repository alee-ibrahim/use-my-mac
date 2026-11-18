// Command relsign makes and checks the minisign signatures on release
// artifacts. The release workflow runs it; it is not shipped.
//
//	relsign keygen -out FILE              write a new unencrypted secret key (0600), print the public key
//	relsign sign -version vX.Y.Z FILE     sign FILE (SHA256SUMS) -> FILE.minisig; key from $UMM_RELEASE_SIGNING_KEY
//	relsign verify -version vX.Y.Z [-pub FILE] FILE
//
// The secret key never goes on the command line: it is read from the
// environment (CI secret) or a file named by -key-file.
package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/alee-ibrahim/use-my-mac/internal/selfupdate"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: relsign keygen|sign|verify ...")
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "keygen":
		err = keygen(os.Args[2:])
	case "sign":
		err = sign(os.Args[2:])
	case "verify":
		err = verify(os.Args[2:])
	default:
		err = fmt.Errorf("unknown command %q", os.Args[1])
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func keygen(args []string) error {
	fs := flag.NewFlagSet("keygen", flag.ExitOnError)
	out := fs.String("out", "", "secret key file to create (mode 0600, must not exist)")
	fs.Parse(args)
	if *out == "" {
		return errors.New("-out is required")
	}
	pub, sk, err := selfupdate.GenerateKey()
	if err != nil {
		return err
	}
	f, err := os.OpenFile(*out, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.WriteString(sk.String()); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	fmt.Print(pub.String())
	return nil
}

func sign(args []string) error {
	fs := flag.NewFlagSet("sign", flag.ExitOnError)
	version := fs.String("version", "", "release version (signed into the trusted comment)")
	keyEnv := fs.String("key-env", "UMM_RELEASE_SIGNING_KEY", "environment variable holding the secret key")
	keyFile := fs.String("key-file", "", "file holding the secret key (instead of -key-env)")
	fs.Parse(args)
	if *version == "" || fs.NArg() != 1 {
		return errors.New("usage: relsign sign -version vX.Y.Z FILE")
	}
	if !selfupdate.IsRelease(*version) {
		return fmt.Errorf("%q is not a release version", *version)
	}
	var raw string
	if *keyFile != "" {
		b, err := os.ReadFile(*keyFile)
		if err != nil {
			return err
		}
		raw = string(b)
	} else {
		raw = os.Getenv(*keyEnv)
	}
	if strings.TrimSpace(raw) == "" {
		return errors.New("no signing key: refusing to publish an unsigned release")
	}
	sk, err := selfupdate.ParseSecretKey(raw)
	if err != nil {
		return err
	}
	msg, err := os.ReadFile(fs.Arg(0))
	if err != nil {
		return err
	}
	sig := sk.Sign(msg, selfupdate.TrustedComment(*version))
	if err := os.WriteFile(fs.Arg(0)+".minisig", sig, 0o644); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "signed %s with key %s\n", fs.Arg(0), selfupdate.KeyIDString(sk.KeyID))
	return nil
}

func verify(args []string) error {
	fs := flag.NewFlagSet("verify", flag.ExitOnError)
	version := fs.String("version", "", "release version")
	pubFile := fs.String("pub", "", "public key file (default: the key built into the agent)")
	fs.Parse(args)
	if *version == "" || fs.NArg() != 1 {
		return errors.New("usage: relsign verify -version vX.Y.Z [-pub FILE] FILE")
	}
	var pub *selfupdate.PublicKey
	var err error
	if *pubFile != "" {
		b, rerr := os.ReadFile(*pubFile)
		if rerr != nil {
			return rerr
		}
		pub, err = selfupdate.ParsePublicKey(string(b))
	} else {
		pub, err = selfupdate.ReleasePublicKey()
	}
	if err != nil {
		return err
	}
	msg, err := os.ReadFile(fs.Arg(0))
	if err != nil {
		return err
	}
	sig, err := os.ReadFile(fs.Arg(0) + ".minisig")
	if err != nil {
		return err
	}
	sums, err := selfupdate.VerifySums(pub, *version, msg, sig)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "ok: %s signed by %s for %s (%d files)\n", fs.Arg(0), selfupdate.KeyIDString(pub.KeyID), *version, len(sums))
	return nil
}
