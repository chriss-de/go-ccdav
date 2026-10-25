// Command ccdav is a small self-hosted CalDAV server backed by plain files.
//
//	ccdav [-config FILE] [-data DIR] [-listen ADDR]   run the server
//	ccdav hash-password                read a password from stdin, print its hash
//	ccdav healthcheck [-listen ADDR]   exit 0 if the local server answers /healthz
//	ccdav share | unshare | shares     manage sharing (see "ccdav share -h")
//
// The config is read from -config, or from config.yaml (or config.yml /
// config.json) in the -data directory. The data directory is -data, else
// data_dir from the config, else the config file's directory. TLS is
// expected to be terminated by a reverse proxy.
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/chriss-de/go-ccdav/internal/auth"
	"github.com/chriss-de/go-ccdav/internal/config"
	"github.com/chriss-de/go-ccdav/internal/directory"
	"github.com/chriss-de/go-ccdav/internal/server"
	"github.com/chriss-de/go-ccdav/internal/store"
	"github.com/chriss-de/go-ccdav/internal/trace"
	"github.com/chriss-de/go-ccdav/internal/web"
)

var version = "dev"

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "hash-password":
			os.Exit(hashPassword())
		case "healthcheck":
			os.Exit(healthcheck(os.Args[2:]))
		case "share", "unshare", "shares":
			os.Exit(shareCmd(os.Args[1], os.Args[2:]))
		case "version", "-version", "--version":
			fmt.Println(version)
			return
		}
	}
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "ccdav:", err)
		os.Exit(1)
	}
}

// env reads CCDAV_<key>, falling back to the pre-rename DAVD_<key>.
func env(key, def string) string {
	for _, k := range []string{"CCDAV_" + key, "DAVD_" + key} {
		if v := os.Getenv(k); v != "" {
			return v
		}
	}
	return def
}

// locate returns the config file: cfgFile if set, else the one found in
// the data directory (default "data").
func locate(data, cfgFile string) (string, error) {
	if cfgFile != "" {
		return cfgFile, nil
	}
	if data == "" {
		data = "data"
	}
	return config.Find(data)
}

// dataDirOf returns data if set, else the config's data_dir.
func dataDirOf(data string, c *config.Config) string {
	if data != "" {
		return data
	}
	return c.DataDir
}

func run() error {
	cfgFlag := flag.String("config", env("CONFIG", ""), "config file (default: config.yaml in the data directory)")
	dataFlag := flag.String("data", env("DATA", ""), "data directory (default: data_dir from the config, else the config file's directory)")
	listen := flag.String("listen", env("LISTEN", ":8080"), "listen address")
	logFormat := flag.String("log", env("LOG", "text"), "log format: text or json")
	traceDir := flag.String("trace", env("TRACE", ""), "record every DAV request and response into this directory (debugging; contains calendar data)")
	flag.Parse()

	var h slog.Handler = slog.NewTextHandler(os.Stderr, nil)
	if *logFormat == "json" {
		h = slog.NewJSONHandler(os.Stderr, nil)
	}
	log := slog.New(h)

	cfgPath, err := locate(*dataFlag, *cfgFlag)
	if err != nil {
		return err
	}
	cfg, err := config.NewStore(cfgPath)
	if err != nil {
		return err
	}
	dataDir := dataDirOf(*dataFlag, cfg.Get())
	st := store.New(dataDir, server.UIDOf)
	// Load everything up front: listing a user's home must know about
	// collections other users shared with them.
	if err := st.LoadAll(); err != nil {
		return err
	}
	if err := directory.Sync(cfg.Get(), st, server.DefaultAddressBooks); err != nil {
		log.Error("directory address books", "err", err)
	}
	key, err := web.LoadOrCreateKey(filepath.Join(dataDir, "session.key"))
	if err != nil {
		return err
	}
	appPass := auth.NewAppPasswords(dataDir)
	defaults := map[store.Kind]map[string]store.Meta{
		store.Calendars:    server.DefaultCalendars,
		store.AddressBooks: server.DefaultAddressBooks,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok\n")) })
	limiter := auth.NewLimiter() // shared: failed DAV and UI logins count together
	ui := web.New(cfg, st, appPass, limiter, key, defaults, log)
	mux.Handle(cfg.Get().UIPrefix, ui)
	mux.HandleFunc("GET /{$}", ui.Landing) // exactly "/"; other methods (PROPFIND) fall through to DAV
	var dav http.Handler = server.New(cfg, st, appPass, limiter, log)
	if *traceDir != "" {
		if dav, err = trace.Handler(*traceDir, dav, log); err != nil {
			return err
		}
		log.Warn("tracing DAV requests; the files contain calendar and contact data", "dir", *traceDir)
	}
	mux.Handle("/", dav) // DAV prefix, well-known; 404 for anything else

	srv := &http.Server{
		Addr:              *listen,
		Handler:           server.Recover(log, cfg, mux),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       2 * time.Minute,
		WriteTimeout:      2 * time.Minute,
		IdleTimeout:       2 * time.Minute,
		ErrorLog:          slog.NewLogLogger(h, slog.LevelWarn),
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	go func() {
		for range hup {
			if err := cfg.Reload(); err != nil {
				log.Error("config reload failed, keeping old config", "err", err)
			} else {
				log.Info("config reloaded")
			}
			// Picks up shares changed with "ccdav share".
			if err := st.ReloadMeta(); err != nil {
				log.Error("reloading collection metadata", "err", err)
			}
			if err := directory.Sync(cfg.Get(), st, server.DefaultAddressBooks); err != nil {
				log.Error("directory address books", "err", err)
			}
		}
	}()

	errc := make(chan error, 1)
	go func() {
		log.Info("listening", "addr", *listen, "data", dataDir, "config", cfgPath,
			"dav_prefix", cfg.Get().DAVPrefix, "ui_prefix", cfg.Get().UIPrefix, "version", version)
		errc <- srv.ListenAndServe()
	}()
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	log.Info("shutting down")
	sctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := srv.Shutdown(sctx); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func hashPassword() int {
	fmt.Fprint(os.Stderr, "password: ")
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && line == "" {
		fmt.Fprintln(os.Stderr, "ccdav:", err)
		return 1
	}
	pw := strings.TrimRight(line, "\r\n")
	if pw == "" {
		fmt.Fprintln(os.Stderr, "ccdav: empty password")
		return 1
	}
	h, err := auth.Hash(pw)
	if err != nil {
		fmt.Fprintln(os.Stderr, "ccdav:", err)
		return 1
	}
	fmt.Println(h)
	return 0
}

func healthcheck(args []string) int {
	fs := flag.NewFlagSet("healthcheck", flag.ContinueOnError)
	listen := fs.String("listen", env("LISTEN", ":8080"), "listen address")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	addr := *listen
	if strings.HasPrefix(addr, ":") {
		addr = "127.0.0.1" + addr
	}
	c := &http.Client{Timeout: 3 * time.Second}
	resp, err := c.Get("http://" + addr + "/healthz")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 1
	}
	return 0
}
