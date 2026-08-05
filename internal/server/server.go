package server

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/base-al/basepod/internal/api"
	"github.com/base-al/basepod/internal/caddy"
	"github.com/base-al/basepod/internal/config"
	"github.com/base-al/basepod/internal/crypto"
	"github.com/base-al/basepod/internal/deploy"
	"github.com/base-al/basepod/internal/podman"
	"github.com/base-al/basepod/internal/store"
)

// Version is the running BasePod version, threaded into the REST API's
// /system endpoint. cmd/basepod sets this from its own build-time Version
// var before calling Run.
var Version = "dev"

// shutdownTimeout bounds how long Run waits for in-flight requests to
// finish once a shutdown signal arrives.
const shutdownTimeout = 10 * time.Second

// Run loads configuration from cfgPath and serves the BasePod control
// plane until ctx is canceled or a SIGINT/SIGTERM is received.
//
// Boot order: load config, open the store, refuse to start if no admin
// user exists yet (run `basepod setup` first), resolve the root domain,
// connect to Podman, ensure the Caddy container is up, build the deploy
// engine, reconcile Caddy's route config against the store's current app
// state (the config file is rebuilt from DB truth on every boot), then
// serve HTTP with a graceful shutdown.
func Run(ctx context.Context, cfgPath string) error {
	ctx, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	cfg, err := config.Load(cfgPath)
	if err != nil {
		return fmt.Errorf("server: load config %s: %w", cfgPath, err)
	}

	st, err := store.Open(filepath.Join(cfg.DataDir, "basepod.db"))
	if err != nil {
		return fmt.Errorf("server: open store: %w", err)
	}
	defer st.Close()

	count, err := st.CountUsers()
	if err != nil {
		return fmt.Errorf("server: count users: %w", err)
	}
	if count == 0 {
		return fmt.Errorf("server: no admin user found — run `basepod setup` first")
	}

	rootDomain, err := st.Setting("root_domain")
	if err != nil {
		return fmt.Errorf("server: read root_domain setting: %w", err)
	}
	if rootDomain == "" {
		rootDomain = cfg.RootDomain
	}
	if rootDomain == "" {
		return fmt.Errorf("server: no root domain configured — run `basepod setup` or set root_domain in the config file")
	}

	pc, err := podman.New(cfg.PodmanSocket)
	if err != nil {
		return fmt.Errorf("server: connect to podman — is Podman running? try `podman machine start`: %w", err)
	}
	if err := pc.Ping(ctx); err != nil {
		return fmt.Errorf("server: podman not reachable — is Podman running? try `podman machine start`: %w", err)
	}

	mgr := caddy.NewManager(pc, caddy.PodmanExec, filepath.Join(cfg.DataDir, "caddy"), cfg.HTTPPort, cfg.HTTPSPort)
	if err := mgr.Ensure(ctx); err != nil {
		return fmt.Errorf("server: ensure caddy: %w", err)
	}

	encKey, err := crypto.LoadOrCreateKey(cfg.DataDir)
	if err != nil {
		return fmt.Errorf("server: load encryption key: %w", err)
	}
	decrypt := func(sealed string) (string, error) {
		return crypto.Open(encKey, sealed)
	}

	engine := deploy.New(st, pc, mgr, deploy.CaddyProber(caddy.PodmanExec), rootDomain, decrypt)

	// Reconcile: the Caddy config file is rebuilt from DB truth on every
	// boot, rather than trusting whatever current.json happened to
	// contain from a previous run.
	routes, err := engine.Routes()
	if err != nil {
		return fmt.Errorf("server: compute routes: %w", err)
	}
	if err := mgr.Apply(ctx, routes); err != nil {
		return fmt.Errorf("server: apply routes: %w", err)
	}

	srv := &http.Server{
		Addr:    cfg.Listen,
		Handler: api.New(st, engine, pc.Ping, Version),
	}

	log.Printf("basepod: listening on %s", cfg.Listen)
	log.Printf("basepod: root domain %s", rootDomain)
	log.Printf("basepod: data dir %s", cfg.DataDir)

	serveErr := make(chan error, 1)
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
			return
		}
		serveErr <- nil
	}()

	select {
	case err := <-serveErr:
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("server: shutdown: %w", err)
		}
		return nil
	}
}
