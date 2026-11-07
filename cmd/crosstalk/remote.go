package main

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"github.com/GoodOlClint/crosstalk/internal/remote"
)

const remoteUsage = `usage: crosstalk remote <command>

commands:
  config [--bind IP] [--port N] [--alias NAME] [--force] [--yes]
             print this host's remote config, or write it
`

func runRemote(args []string, in io.Reader, tty bool, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] != "config" {
		if len(args) > 0 {
			fmt.Fprintf(stderr, "crosstalk remote: unknown command %q\n", args[0])
		}
		fmt.Fprint(stderr, remoteUsage)
		return 2
	}
	return runRemoteConfig(args[1:], in, tty, stdout, stderr)
}

func runRemoteConfig(args []string, in io.Reader, tty bool, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("remote config", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	bind := fs.String("bind", "", "")
	port := fs.Int("port", 0, "")
	alias := fs.String("alias", "", "")
	force := fs.Bool("force", false, "")
	yes := fs.Bool("yes", false, "")
	if err := fs.Parse(args); err != nil || fs.NArg() > 0 {
		if err == nil {
			err = fmt.Errorf("unexpected argument %q", fs.Arg(0))
		}
		fmt.Fprintf(stderr, "crosstalk remote config: %v\n%s", err, remoteUsage)
		return 2
	}
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
	dir, err := remote.ConfigDir()
	if err != nil {
		fmt.Fprintf(stderr, "crosstalk remote config: %v\n", err)
		return 1
	}
	old, err := remote.LoadConfig(dir)
	configured := err == nil
	if err != nil && !errors.Is(err, remote.ErrNotConfigured) {
		fmt.Fprintf(stderr, "crosstalk remote config: %v\n", err)
		return 1
	}
	if !set["bind"] && !set["port"] && !set["alias"] {
		return remoteShowConfig(dir, old, configured, stdout, stderr)
	}

	apply := func(cur remote.Config, have bool) (remote.Config, error) {
		next := cur
		if !have {
			if !set["bind"] {
				return next, errUsage("--bind IP is required the first time")
			}
			next = remote.Config{Alias: remote.DefaultAlias(), Port: remote.DefaultPort}
		}
		if set["bind"] {
			b, err := remote.ValidateBind(*bind, *force)
			if err != nil {
				return next, errUsage("--bind: " + err.Error())
			}
			next.Bind = b
		}
		if set["port"] {
			if *port < 1 || *port > 65535 {
				return next, errUsage(fmt.Sprintf("--port %d is outside 1-65535", *port))
			}
			next.Port = *port
		}
		if set["alias"] {
			if err := remote.ValidAlias(*alias); err != nil {
				return next, errUsage("--alias: " + err.Error())
			}
			next.Alias = *alias
		}
		return next, nil
	}
	next, err := apply(old, configured)
	if err != nil {
		return remoteFail(err, stderr)
	}
	var rePair []string
	if configured && (next.Bind != old.Bind || next.Port != old.Port) {
		ps, err := remote.NewPeers(dir).List()
		if err != nil {
			fmt.Fprintf(stderr, "crosstalk remote config: %v\n", err)
			return 1
		}
		for _, p := range ps {
			rePair = append(rePair, p.Alias)
		}
	}

	fmt.Fprintf(stdout, "will write %s:\n  alias %s\n  bind  %s\n  port  %d\n", filepath.Join(dir, "remote.json"), next.Alias, next.Bind, next.Port)
	if _, err := remote.LoadHost(dir); errors.Is(err, remote.ErrNotConfigured) {
		fmt.Fprintf(stdout, "will create %s and %s\n", filepath.Join(dir, "host.key"), filepath.Join(dir, "host.crt"))
	}
	if !*yes {
		if !tty {
			fmt.Fprintln(stderr, "crosstalk remote config: no terminal to confirm on; nothing changed. Run it in a terminal or pass --yes.")
			return 2
		}
		fmt.Fprint(stdout, "Write? [y/N] ")
		line, _ := bufio.NewReader(in).ReadString('\n')
		if a := strings.ToLower(strings.TrimSpace(line)); a != "y" && a != "yes" {
			fmt.Fprintln(stderr, "stopped: declined")
			return 1
		}
	}

	h, err := remote.LoadOrCreateHost(dir)
	if err != nil {
		return remoteFail(err, stderr)
	}
	next, err = remote.UpdateConfig(dir, apply)
	if err != nil {
		return remoteFail(err, stderr)
	}
	printConfig(stdout, dir, next, h.Fingerprint)
	if len(rePair) > 0 {
		fmt.Fprintf(stdout, "peers still dial the old address: pair again with %s\n", strings.Join(rePair, ", "))
	}
	return 0
}

type errUsage string

func (e errUsage) Error() string { return string(e) }

func remoteFail(err error, stderr io.Writer) int {
	fmt.Fprintf(stderr, "crosstalk remote config: %v\n", err)
	if errors.As(err, new(errUsage)) {
		return 2
	}
	return 1
}

func remoteShowConfig(dir string, c remote.Config, configured bool, stdout, stderr io.Writer) int {
	if !configured {
		fmt.Fprintln(stdout, "not configured; run: crosstalk remote config --bind IP")
		return 0
	}
	fp := "none yet"
	switch h, err := remote.LoadHost(dir); {
	case err == nil:
		fp = h.Fingerprint
	case !errors.Is(err, remote.ErrNotConfigured):
		fmt.Fprintf(stderr, "crosstalk remote config: %v\n", err)
		return 1
	}
	printConfig(stdout, dir, c, fp)
	return 0
}

func printConfig(w io.Writer, dir string, c remote.Config, fp string) {
	fmt.Fprintf(w, "alias:       %s\nbind:        %s\nport:        %d\nfingerprint: %s\nconfig:      %s\n",
		c.Alias, c.Bind, c.Port, fp, filepath.Join(dir, "remote.json"))
}
