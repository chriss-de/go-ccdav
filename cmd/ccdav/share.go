package main

import (
	"flag"
	"fmt"
	"os"
	"slices"
	"strings"

	"github.com/chriss-de/go-ccdav/internal/config"
	"github.com/chriss-de/go-ccdav/internal/server"
	"github.com/chriss-de/go-ccdav/internal/store"
)

const shareUsage = `usage:
  ccdav share   [-data DIR] <owner>/<calendars|addressbooks>/<name> user:<u>|group:<g> read|write
  ccdav unshare [-data DIR] <owner>/<calendars|addressbooks>/<name> user:<u>|group:<g>
  ccdav shares  [-data DIR] [owner]

Changes are written to the collection's .meta.json. A running server picks
them up on SIGHUP (kill -HUP <pid>, podman kill -s HUP <container>).
`

func shareCmd(cmd string, args []string) int {
	fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
	dataDir := fs.String("data", env("DAVD_DATA", "data"), "data directory")
	fs.Usage = func() { fmt.Fprint(os.Stderr, shareUsage) }
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if err := runShare(cmd, *dataDir, fs.Args()); err != nil {
		fmt.Fprintln(os.Stderr, "ccdav:", err)
		if strings.HasPrefix(err.Error(), "usage") {
			fmt.Fprint(os.Stderr, shareUsage)
		}
		return 1
	}
	return 0
}

func runShare(cmd, dataDir string, args []string) error {
	cfgPath, err := config.Find(dataDir)
	if err != nil {
		return err
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return err
	}
	st := store.New(dataDir, server.UIDOf)

	if cmd == "shares" {
		if len(args) > 1 {
			return fmt.Errorf("usage")
		}
		if err := st.LoadAll(); err != nil {
			return err
		}
		for _, c := range st.All() {
			if len(args) == 1 && c.Owner != args[0] {
				continue
			}
			for _, sh := range c.Shares() {
				who := "user:" + sh.User
				if sh.Group != "" {
					who = "group:" + sh.Group
				}
				fmt.Printf("%-40s %-20s %s\n", c.Key(), who, sh.Access)
			}
		}
		return nil
	}

	want := 3
	if cmd == "unshare" {
		want = 2
	}
	if len(args) != want {
		return fmt.Errorf("usage: %s takes %d arguments", cmd, want)
	}
	col, err := openCollection(st, args[0])
	if err != nil {
		return err
	}
	target, err := parseTarget(cfg, args[1], col.Owner)
	if err != nil {
		return err
	}
	shares := slices.DeleteFunc(col.Shares(), func(sh store.Share) bool {
		return sh.User == target.User && sh.Group == target.Group
	})
	if cmd == "share" {
		if args[2] != store.AccessRead && args[2] != store.AccessWrite {
			return fmt.Errorf("access must be %q or %q", store.AccessRead, store.AccessWrite)
		}
		target.Access = args[2]
		shares = append(shares, target)
	}
	if err := col.SetShares(shares); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "%s: %d share(s). Send SIGHUP to a running server to apply.\n", col.Key(), len(shares))
	return nil
}

func openCollection(st *store.Store, key string) (*store.Collection, error) {
	parts := strings.Split(key, "/")
	if len(parts) != 3 {
		return nil, fmt.Errorf("collection must be <owner>/<calendars|addressbooks>/<name>, got %q", key)
	}
	kind := store.Kind(parts[1])
	if kind != store.Calendars && kind != store.AddressBooks {
		return nil, fmt.Errorf("unknown kind %q", parts[1])
	}
	c, err := st.Collection(parts[0], kind, parts[2])
	if err != nil {
		return nil, fmt.Errorf("%s: %w", key, err)
	}
	return c, nil
}

func parseTarget(cfg *config.Config, s, owner string) (store.Share, error) {
	typ, name, ok := strings.Cut(s, ":")
	switch {
	case ok && typ == "user":
		if _, known := cfg.Users[name]; !known {
			return store.Share{}, fmt.Errorf("unknown user %q", name)
		}
		if name == owner {
			return store.Share{}, fmt.Errorf("%s owns the collection", name)
		}
		return store.Share{User: name}, nil
	case ok && typ == "group":
		if _, known := cfg.Groups[name]; !known {
			return store.Share{}, fmt.Errorf("unknown group %q", name)
		}
		return store.Share{Group: name}, nil
	}
	return store.Share{}, fmt.Errorf("target must be user:<name> or group:<name>, got %q", s)
}
