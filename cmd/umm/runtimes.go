package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/alee-ibrahim/use-my-mac/internal/runtimes"
	"github.com/alee-ibrahim/use-my-mac/internal/selfupdate"
	"github.com/alee-ibrahim/use-my-mac/pkg/api"
)

// runtimesCmd manages the managed runtime catalog. sign and keygen work
// offline (no router needed); the secret key never goes on the command line.
func runtimesCmd(ctx context.Context, args []string) error {
	sub := "list"
	if len(args) > 0 {
		sub, args = args[0], args[1:]
	}
	fs := flag.NewFlagSet("runtimes "+sub, flag.ExitOnError)
	switch sub {
	case "keygen":
		out := fs.String("out", "", "secret key file to create (mode 0600, must not exist)")
		parseFlags(fs, args)
		if *out == "" {
			return errors.New("usage: umm runtimes keygen -out FILE")
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
		fmt.Fprintln(os.Stderr, "Public key (put it in agent.yaml managed_runtimes.public_keys):")
		fmt.Print(pub.String())
		return nil
	case "sign":
		keyFile := fs.String("key-file", "", "minisign secret key file")
		keyEnv := fs.String("key-env", "UMM_RUNTIME_SIGNING_KEY", "environment variable holding the secret key (if no -key-file)")
		pos := parseFlags(fs, args)
		if len(pos) != 1 {
			return errors.New("usage: umm runtimes sign MANIFEST [-key-file F]")
		}
		raw := os.Getenv(*keyEnv)
		if *keyFile != "" {
			b, err := os.ReadFile(*keyFile)
			if err != nil {
				return err
			}
			raw = string(b)
		}
		if strings.TrimSpace(raw) == "" {
			return errors.New("no signing key: pass -key-file or set " + *keyEnv)
		}
		sk, err := selfupdate.ParseSecretKey(raw)
		if err != nil {
			return err
		}
		m, err := os.ReadFile(pos[0])
		if err != nil {
			return err
		}
		sig, err := runtimes.Sign(sk, m)
		if err != nil {
			return err
		}
		if err := os.WriteFile(pos[0]+".minisig", sig, 0o644); err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "signed %s with key %s -> %s.minisig\n", pos[0], selfupdate.KeyIDString(sk.KeyID), pos[0])
		return nil
	}

	c, err := newClient()
	if err != nil {
		return err
	}
	switch sub {
	case "list", "ls":
		parseFlags(fs, args)
		rc, err := c.Runtimes(ctx)
		if err != nil {
			return err
		}
		w := tw()
		fmt.Fprintln(w, "NAME\tVERSION\tCOMMANDS\tPLATFORMS\tPREWARM\tINSTALLED\tINSTALLABLE\tFAILED")
		for _, e := range rc.Packages {
			n := map[string]int{}
			for _, m := range e.Macs {
				n[m.State]++
			}
			var cmds, plats []string
			for k := range e.Package.Commands {
				cmds = append(cmds, k)
			}
			sort.Strings(cmds)
			for _, a := range e.Package.Artifacts {
				plats = append(plats, a.OS+"/"+a.Arch)
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%v\t%d\t%d\t%d\n", e.Package.Name, e.Package.Version, strings.Join(cmds, ","),
				strings.Join(plats, ","), e.Prewarm, n[api.PackageInstalled], n[api.PackageInstallable]+n[api.PackageOutdated],
				n[api.PackageFailed])
		}
		return w.Flush()
	case "show":
		pos := parseFlags(fs, args)
		if len(pos) != 1 {
			return errors.New("usage: umm runtimes show NAME")
		}
		rc, err := c.Runtimes(ctx)
		if err != nil {
			return err
		}
		for _, e := range rc.Packages {
			if e.Package.Name != pos[0] {
				continue
			}
			fmt.Printf("%s %s (key %s, published %s by %s, prewarm %v)\n", e.Package.Name, e.Package.Version, e.KeyID,
				e.PublishedAt.Local().Format("2006-01-02 15:04"), dash(e.PublishedBy), e.Prewarm)
			w := tw()
			fmt.Fprintln(w, "MAC\tSTATE\tVERSION\tERROR")
			for _, m := range e.Macs {
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", m.Name, m.State, dash(m.Version), dash(m.Error))
			}
			return w.Flush()
		}
		return fmt.Errorf("runtime %s is not in the catalog", pos[0])
	case "upload":
		tf := addTransferFlags(fs)
		pos := parseFlags(fs, args)
		if len(pos) != 2 {
			return errors.New("usage: umm runtimes upload FILE KEY")
		}
		f, err := os.Open(pos[0])
		if err != nil {
			return err
		}
		h := sha256.New()
		n, err := io.Copy(h, f)
		f.Close()
		if err != nil {
			return err
		}
		if err := upload(ctx, c, pos[1], pos[0], tf); err != nil {
			return err
		}
		fmt.Printf("\"blob\": %q, \"sha256\": %q, \"size\": %d\n", pos[1], hex.EncodeToString(h.Sum(nil)), n)
		return nil
	case "publish":
		sigFile := fs.String("sig", "", "signature file (default MANIFEST.minisig)")
		prewarm := fs.Bool("prewarm", false, "install on every Mac that can, before tasks need it")
		pos := parseFlags(fs, args)
		if len(pos) != 1 {
			return errors.New("usage: umm runtimes publish MANIFEST [-sig F] [-prewarm]")
		}
		if *sigFile == "" {
			*sigFile = pos[0] + ".minisig"
		}
		m, err := os.ReadFile(pos[0])
		if err != nil {
			return err
		}
		sig, err := os.ReadFile(*sigFile)
		if err != nil {
			return fmt.Errorf("%w (sign it first: umm runtimes sign %s -key-file KEY)", err, pos[0])
		}
		e, err := c.PublishRuntime(ctx, api.RuntimePublish{Manifest: string(m), Signature: string(sig), Prewarm: *prewarm})
		if err != nil {
			return err
		}
		fmt.Printf("published %s %s (key %s)\n", e.Package.Name, e.Package.Version, e.KeyID)
		return nil
	case "prewarm":
		off := fs.Bool("off", false, "stop prewarming")
		pos := parseFlags(fs, args)
		if len(pos) != 1 {
			return errors.New("usage: umm runtimes prewarm NAME [-off]")
		}
		return c.SetRuntimePrewarm(ctx, pos[0], !*off)
	case "rm", "delete":
		pos := parseFlags(fs, args)
		if len(pos) != 1 {
			return errors.New("usage: umm runtimes rm NAME")
		}
		return c.DeleteRuntime(ctx, pos[0])
	}
	return fmt.Errorf("unknown runtimes command %q", sub)
}
