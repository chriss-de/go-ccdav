// Command davd is a small self-hosted CalDAV server backed by plain files.
//
//	davd [-data DIR] [-listen ADDR]   run the server
//	davd hash-password                read a password from stdin, print its hash
//	davd healthcheck [-listen ADDR]   exit 0 if the local server answers /healthz
//
// The config is read from config.yaml (or config.yml / config.json) in the
// data directory. TLS is expected to be terminated by a reverse proxy.
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
	"strings"
	"syscall"
	"time"

	"davd/internal/auth"
	"davd/internal/config"
	"davd/internal/server"
	"davd/internal/store"
)

var version = "dev"

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "hash-password":
			os.Exit(hashPassword())
		case "healthcheck":
			os.Exit(healthcheck(os.Args[2:]))
		case "version", "-version", "--version":
			fmt.Println(version)
			return
		}
	}
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "davd:", err)
		os.Exit(1)
	}
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func run() error {
	dataDir := flag.String("data", env("DAVD_DATA", "data"), "data directory (holds config.yaml)")
	listen := flag.String("listen", env("DAVD_LISTEN", ":8080"), "listen address")
	logFormat := flag.String("log", env("DAVD_LOG", "text"), "log format: text or json")
	flag.Parse()

	var h slog.Handler = slog.NewTextHandler(os.Stderr, nil)
	if *logFormat == "json" {
		h = slog.NewJSONHandler(os.Stderr, nil)
	}
	log := slog.New(h)

	cfgPath, err := config.Find(*dataDir)
	if err != nil {
		return err
	}
	cfg, err := config.NewStore(cfgPath)
	if err != nil {
		return err
	}
	st := store.New(*dataDir, server.UIDOf)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok\n")) })
	mux.Handle("/", server.New(cfg, st, log))

	srv := &http.Server{
		Addr:              *listen,
		Handler:           mux,
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
		}
	}()

	errc := make(chan error, 1)
	go func() {
		log.Info("listening", "addr", *listen, "data", *dataDir, "config", cfgPath, "version", version)
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
		fmt.Fprintln(os.Stderr, "davd:", err)
		return 1
	}
	pw := strings.TrimRight(line, "\r\n")
	if pw == "" {
		fmt.Fprintln(os.Stderr, "davd: empty password")
		return 1
	}
	h, err := auth.Hash(pw)
	if err != nil {
		fmt.Fprintln(os.Stderr, "davd:", err)
		return 1
	}
	fmt.Println(h)
	return 0
}

func healthcheck(args []string) int {
	fs := flag.NewFlagSet("healthcheck", flag.ContinueOnError)
	listen := fs.String("listen", env("DAVD_LISTEN", ":8080"), "listen address")
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
