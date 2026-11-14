// Command traceforge runs the TraceForge server: the OTLP/HTTP trace
// receiver, the detection and investigation engines, the REST API and the
// web interface.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/TrulyAchyuth/TraceForge/internal/api"
	"github.com/TrulyAchyuth/TraceForge/internal/config"
	"github.com/TrulyAchyuth/TraceForge/internal/detect"
	"github.com/TrulyAchyuth/TraceForge/internal/ingest"
	"github.com/TrulyAchyuth/TraceForge/internal/investigate"
	"github.com/TrulyAchyuth/TraceForge/internal/lab"
	"github.com/TrulyAchyuth/TraceForge/internal/memory"
	"github.com/TrulyAchyuth/TraceForge/internal/store"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "traceforge:", err)
		os.Exit(1)
	}
}

// Background task cadences.
const (
	edgeResolveInterval = 2 * time.Second
	// edgeGrace is how long a client span waits for its server span before
	// the edge is resolved from the client span alone.
	edgeGrace        = 5 * time.Second
	edgeResolveBatch = 5000
	purgeInterval    = 10 * time.Minute
	dbConnectTimeout = 60 * time.Second
)

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("configuration: %w", err)
	}
	log := newLogger(cfg)
	slog.SetDefault(log)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	st, err := connect(ctx, cfg.DatabaseURL, log)
	if err != nil {
		return err
	}
	defer st.Close()
	if err := st.Migrate(ctx); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	log.Info("database ready")

	targets, err := lab.ParseTargets(cfg.LabTargets)
	if err != nil {
		return fmt.Errorf("configuration: %w", err)
	}

	icfg := ingest.DefaultConfig()
	icfg.Token, icfg.MaxQueuedSpans = cfg.IngestToken, cfg.MaxQueuedSpans
	icfg.Limits.MaxAge = time.Duration(cfg.RetentionHours) * time.Hour
	pipeline := ingest.New(icfg, st, log)
	inv := investigate.New(st, log)
	engine := detect.NewEngine(st, inv, cfg.DetectInterval, log)
	if err := engine.LoadConfig(ctx); err != nil {
		return fmt.Errorf("load detection config: %w", err)
	}
	labMgr := lab.NewManager(st, targets, log)
	labMgr.SetToken(cfg.LabToken)
	mem := memory.NewService(st, inv, log)

	// Background work outlives the signal context: it is cancelled only after
	// the HTTP servers have stopped, so spans accepted during shutdown are
	// still written.
	bgCtx, bgCancel := context.WithCancel(context.Background())
	defer bgCancel()
	var wg sync.WaitGroup
	background := func(name string, fn func(context.Context)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			fn(bgCtx)
			log.Debug("background task stopped", "task", name)
		}()
	}
	background("ingest-writer", pipeline.Run)
	background("detection", engine.Run)
	background("lab-watcher", labMgr.Run)
	background("incident-memory", mem.Run)
	background("edge-resolver", func(ctx context.Context) {
		every(ctx, edgeResolveInterval, func() {
			for {
				n, err := st.ResolveEdges(ctx, edgeGrace, edgeResolveBatch)
				if err != nil {
					if ctx.Err() == nil {
						log.Warn("edge resolution failed", "error", err)
					}
					return
				}
				if n < edgeResolveBatch {
					return
				}
			}
		})
	})
	background("retention", func(ctx context.Context) {
		every(ctx, purgeInterval, func() {
			cutoff := time.Now().Add(-time.Duration(cfg.RetentionHours) * time.Hour)
			n, err := st.Purge(ctx, cutoff)
			if err != nil {
				if ctx.Err() == nil {
					log.Warn("retention purge failed", "error", err)
				}
				return
			}
			if n > 0 {
				log.Info("retention purge", "rows_deleted", n, "cutoff", cutoff.UTC())
			}
		})
	})

	apiSrv := api.New(api.Options{Store: st, Pipeline: pipeline, Engine: engine, Investigations: inv, Lab: labMgr, Memory: mem, Log: log,
		Token: cfg.APIToken, IngestToken: cfg.IngestToken != "", WebDir: cfg.WebDir,
		RetentionHours: cfg.RetentionHours, DetectInterval: cfg.DetectInterval})

	otlpMux := http.NewServeMux()
	otlpMux.Handle("/v1/traces", pipeline)
	otlpMux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "TraceForge accepts OTLP/HTTP traces at POST /v1/traces. Metrics and logs are not ingested.", http.StatusNotFound)
	})

	servers := []*http.Server{
		newServer(cfg.HTTPAddr, apiSrv.Handler(), 60*time.Second),
		newServer(cfg.OTLPAddr, otlpMux, 30*time.Second),
	}
	errCh := make(chan error, len(servers))
	for _, srv := range servers {
		go func() {
			log.Info("listening", "addr", srv.Addr)
			if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				errCh <- fmt.Errorf("listen %s: %w", srv.Addr, err)
			}
		}()
	}

	select {
	case <-ctx.Done():
		log.Info("shutdown requested")
	case err := <-errCh:
		stop()
		log.Error("server failed", "error", err)
		shutdown(servers, cfg.ShutdownTimeout, log)
		bgCancel()
		wg.Wait()
		return err
	}
	shutdown(servers, cfg.ShutdownTimeout, log)
	bgCancel()
	wg.Wait()
	log.Info("stopped")
	return nil
}

func newServer(addr string, h http.Handler, writeTimeout time.Duration) *http.Server {
	return &http.Server{Addr: addr, Handler: h, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 30 * time.Second,
		WriteTimeout: writeTimeout, IdleTimeout: 2 * time.Minute, MaxHeaderBytes: 1 << 16}
}

func shutdown(servers []*http.Server, timeout time.Duration, log *slog.Logger) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	for _, srv := range servers {
		if err := srv.Shutdown(ctx); err != nil {
			log.Warn("server shutdown", "addr", srv.Addr, "error", err)
		}
	}
}

// every runs fn immediately and then on each interval until ctx ends.
func every(ctx context.Context, interval time.Duration, fn func()) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		fn()
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// connect opens the database, retrying while it starts up.
func connect(ctx context.Context, url string, log *slog.Logger) (*store.Store, error) {
	st, err := store.Open(ctx, url)
	if err != nil {
		return nil, fmt.Errorf("database: %w", err)
	}
	deadline := time.Now().Add(dbConnectTimeout)
	for {
		pingCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		err := st.Ping(pingCtx)
		cancel()
		if err == nil {
			return st, nil
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			st.Close()
			return nil, fmt.Errorf("database not reachable after %s: %w", dbConnectTimeout, err)
		}
		log.Warn("waiting for database", "error", err)
		select {
		case <-time.After(2 * time.Second):
		case <-ctx.Done():
		}
	}
}

func newLogger(cfg config.Config) *slog.Logger {
	level := map[string]slog.Level{"debug": slog.LevelDebug, "info": slog.LevelInfo, "warn": slog.LevelWarn, "error": slog.LevelError}[cfg.LogLevel]
	opts := &slog.HandlerOptions{Level: level}
	if cfg.LogFormat == "text" {
		return slog.New(slog.NewTextHandler(os.Stderr, opts))
	}
	return slog.New(slog.NewJSONHandler(os.Stderr, opts))
}
