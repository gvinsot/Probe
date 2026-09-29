// Command probe-hub serves the Probe web application: forge sign-in,
// policy bootstrap, push monitoring and the dynamic report viewer.
//
// It is one container with no database and no queue of its own, so a company
// can run it next to its GitHub Enterprise or GitLab instance.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/gvinsot/Probe/hub/internal/accounts"
	"github.com/gvinsot/Probe/hub/internal/analysis"
	"github.com/gvinsot/Probe/hub/internal/config"
	"github.com/gvinsot/Probe/hub/internal/events"
	"github.com/gvinsot/Probe/hub/internal/forge"
	"github.com/gvinsot/Probe/hub/internal/secrets"
	"github.com/gvinsot/Probe/hub/internal/server"
	"github.com/gvinsot/Probe/hub/internal/store"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "version", "--version", "-v":
			fmt.Printf("probe-hub %s\nAGPL-3.0 with an attribution term, see NOTICE: https://github.com/gvinsot/Probe\n", version)
			return
		case "healthcheck":
			// Used by the container HEALTHCHECK so the image needs no curl.
			if err := healthcheck(); err != nil {
				fmt.Fprintf(os.Stderr, "probe-hub: %v\n", err)
				os.Exit(1)
			}
			return
		case "help", "--help", "-h":
			fmt.Print(usage)
			return
		default:
			fmt.Fprintf(os.Stderr, "unknown command %q\n%s", os.Args[1], usage)
			os.Exit(2)
		}
	}
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "probe-hub: %v\n", err)
		os.Exit(1)
	}
}

const usage = `probe-hub — the Probe web application

The service is configured through the environment:

  PROBE_HUB_BASE_URL            public URL of this deployment (required)
  PROBE_HUB_ADDR                listen address (default :8080)
  PROBE_HUB_DATA_DIR            state directory (default /var/lib/probe-hub)
  PROBE_HUB_SESSION_KEY         64 hex characters; generated and persisted when unset
  PROBE_HUB_SESSION_KEY_PREVIOUS keys retired by a rotation, still able to open
                                     stored credentials, which are resealed at start-up
  PROBE_HUB_INSTANCE            public (default) or private; a public instance
                                     permits lint and read-only AI review
  PROBE_HUB_MODE                auto (default), lint, review-read-only or review;
                                     full review requires a private instance
                                     auto uses read-only AI review with an endpoint
                                     and model configured, otherwise lint
  PROBE_HUB_REVIEW_POLICIES     review allowlist, entries
                                     <github|gitlab>:<owner/repo>@sha256:<policy digest>;
                                     every other repository is linted
  PROBE_HUB_WORKERS             concurrent analyses (default 2)
  PROBE_HUB_USER_QUOTA          analyses one account may have queued or running (default 8)
  PROBE_HUB_HOOK_RATE           webhook deliveries per routing key and minute (default 30)

Any *_SECRET, *_KEY, *_KEY_PREVIOUS or *_POLICIES value can be read from the
file named by <NAME>_FILE, or from /run/secrets/<NAME>.
  PROBE_HUB_DEFAULT_BRANCH_ONLY analyze only the default branch (default false)
  PROBE_HUB_COMMIT_STATUS       publish the verdict on the commit (default true)

  PROBE_HUB_GITHUB_CLIENT_ID / _SECRET [, _URL, _API_URL, _SCOPES]
  PROBE_HUB_GITLAB_CLIENT_ID / _SECRET [, _URL, _SCOPES]
  PROBE_HUB_ALLOW_NO_FORGE      serve without sign-in while no forge is
                                     configured (default false)

Commands: no argument serves the application, "healthcheck" probes /healthz
from inside the container, "version" prints the build.

At least one forge must be configured, unless the deployment opts into
starting without sign-in. See hub/README.md.
`

func run() error {
	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level()}))
	slog.SetDefault(log)

	cfg, err := config.Load(os.Getenv)
	if err != nil {
		return err
	}
	keys, err := secrets.New(cfg.SessionKey)
	if err != nil {
		return err
	}
	if err := keys.WithPrevious(cfg.PreviousSessionKeys); err != nil {
		return err
	}
	st, err := store.Open(cfg.DataDir)
	if err != nil {
		return err
	}
	if len(cfg.PreviousSessionKeys) > 0 {
		// After a rotation, move every stored credential to the new key so
		// the previous one can be dropped at the next restart.
		moved, err := accounts.Rewrap(st, keys)
		if err != nil {
			return fmt.Errorf("reseal stored credentials: %w", err)
		}
		log.Info("session key rotation", "resealed", moved)
	}
	// Hooks installed before installation tokens are refused: say so once at
	// start-up; each owner sees the action to take on the dashboard.
	if outdated, err := st.OutdatedHooks(); err != nil {
		log.Warn("count outdated webhooks", "error", err)
	} else if outdated > 0 {
		log.Warn("webhooks predate installation tokens and are refused until reinstalled", "repositories", outdated)
	}
	providers := map[string]forge.Provider{}
	for kind, f := range cfg.Forges {
		switch kind {
		case config.GitHub:
			providers[kind] = forge.NewGitHub(f)
		case config.GitLab:
			providers[kind] = forge.NewGitLab(f)
		}
	}
	acct := accounts.New(st, keys, providers)
	broker := events.New()
	runner := analysis.New(cfg, st, acct, broker, log)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	runner.Start(ctx)

	srv, err := server.New(cfg, st, acct, runner, broker, keys, log, version)
	if err != nil {
		return err
	}
	httpServer := &http.Server{
		Addr:              cfg.Addr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       60 * time.Second,
		// No write timeout: the dashboard holds an open event stream.
		IdleTimeout: 120 * time.Second,
	}
	forges := make([]string, 0, len(providers))
	for kind := range providers {
		forges = append(forges, kind)
	}
	if len(forges) == 0 {
		log.Warn("no forge configured: the application serves, but nobody can sign in until " +
			"PROBE_HUB_GITHUB_CLIENT_ID/_SECRET or the GitLab pair is set")
	}
	log.Info("starting", "version", version, "addr", cfg.Addr, "base_url", cfg.BaseURL,
		"instance", cfg.Instance, "mode", cfg.Mode, "review_policies", len(cfg.ReviewPolicies), "workers", cfg.Workers, "forges", forges, "cli", runner.Version(ctx))

	errs := make(chan error, 1)
	go func() {
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errs <- err
		}
	}()
	select {
	case err := <-errs:
		return err
	case <-ctx.Done():
		log.Info("shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		return httpServer.Shutdown(shutdownCtx)
	}
}

// healthcheck probes the local /healthz endpoint of this container.
func healthcheck() error {
	addr := os.Getenv("PROBE_HUB_ADDR")
	if addr == "" {
		addr = ":8080"
	}
	if strings.HasPrefix(addr, ":") {
		addr = "127.0.0.1" + addr
	}
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get("http://" + addr + "/healthz")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("/healthz returned %d", resp.StatusCode)
	}
	return nil
}

func level() slog.Level {
	switch os.Getenv("PROBE_HUB_LOG_LEVEL") {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
