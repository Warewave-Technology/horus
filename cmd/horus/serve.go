package main

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/Warewave-Technology/horus/internal/config"
	"github.com/Warewave-Technology/horus/internal/dnsserver"
	"github.com/Warewave-Technology/horus/internal/httpapi"
	"github.com/Warewave-Technology/horus/internal/metrics"
	"github.com/Warewave-Technology/horus/internal/sdnotify"
	"github.com/Warewave-Technology/horus/internal/store"
	"github.com/Warewave-Technology/horus/internal/zone"
)

func runServe() error {
	cfg, err := config.FromEnv(os.Getenv)
	if err != nil {
		return err
	}
	log := newLogger(cfg.LogLevel)
	slog.SetDefault(log)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// ---- store + snapshot
	if err := os.MkdirAll(filepath.Dir(cfg.DBPath), 0o700); err != nil {
		return err
	}
	st, err := store.Open(ctx, sqliteDriver, store.DSN(cfg.DBPath))
	if err != nil {
		return fmt.Errorf("open db %s: %w", cfg.DBPath, err)
	}
	defer st.Close()

	holder := zone.NewHolder()
	reload := func(ctx context.Context) error {
		zones, recs, err := st.LoadAll(ctx)
		if err != nil {
			return err
		}
		snap := zone.Build(zones, recs)
		holder.Store(snap)
		log.Debug("snapshot reloaded", "zones", snap.Zones(), "records", snap.Len())
		return nil
	}
	if err := reload(ctx); err != nil {
		return fmt.Errorf("initial snapshot: %w", err)
	}

	// ---- metrics
	reg := metrics.NewRegistry()
	reg.NewGaugeFunc("horus_zones", "Zones in the served snapshot.", func() float64 { return float64(holder.Load().Zones()) })
	reg.NewGaugeFunc("horus_records", "Records in the served snapshot.", func() float64 { return float64(holder.Load().Len()) })

	// ---- DNS
	fwd := dnsserver.NewForwarder(dnsserver.ForwarderOptions{
		Upstreams: cfg.Upstreams,
		Timeout:   cfg.UpstreamTimeout,
		CacheSize: cfg.CacheMaxEntries,
		MaxTTL:    cfg.CacheMaxTTL,
		NegTTL:    cfg.CacheNegTTL,
		Log:       log,
		Metrics:   reg,
	})
	handler := dnsserver.NewHandler(dnsserver.Options{
		Snapshots: holder,
		Forwarder: fwd,
		SOAMName:  cfg.SOAMName,
		SOARName:  cfg.SOARName,
		Log:       log,
		QueryLog:  cfg.QueryLog,
		Metrics:   reg,
	})
	dnsLn, err := dnsserver.Listen(cfg.DNSListen, handler)
	if err != nil {
		err = fmt.Errorf("dns listen %s: %w", cfg.DNSListen, err)
		if _, port, _ := net.SplitHostPort(cfg.DNSListen); port == "53" && errors.Is(err, syscall.EADDRINUSE) {
			err = fmt.Errorf("%w (is systemd-resolved's stub listener on :53? see docs/DESIGN.md)", err)
		}
		return err
	}

	// ---- API
	api := httpapi.New(httpapi.Options{
		Store:      st,
		AdminToken: cfg.AdminToken,
		Reload:     reload,
		BackupDir:  cfg.BackupDir,
		Log:        log,
		Public:     map[string]http.Handler{"/metrics": reg.Handler()},
	})
	httpErr := make(chan error, 2)
	var httpServers []*http.Server
	newHTTP := func() *http.Server {
		s := &http.Server{
			Handler:           api.Handler(),
			ReadHeaderTimeout: 5 * time.Second,
			ReadTimeout:       30 * time.Second,
			WriteTimeout:      30 * time.Second,
			IdleTimeout:       2 * time.Minute,
			ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelWarn),
		}
		httpServers = append(httpServers, s)
		return s
	}
	if cfg.APISocket != "" {
		l, err := listenUnix(cfg.APISocket)
		if err != nil {
			return err
		}
		s := newHTTP()
		go func() { httpErr <- s.Serve(l) }()
	}
	if cfg.APIListen != "" {
		// Load the key pair before binding so a bad certificate fails the
		// start instead of surfacing after READY=1.
		var tlsConf *tls.Config
		if cfg.TLSCert != "" {
			cert, err := tls.LoadX509KeyPair(cfg.TLSCert, cfg.TLSKey)
			if err != nil {
				return fmt.Errorf("api tls: %w", err)
			}
			tlsConf = &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
		}
		l, err := net.Listen("tcp", cfg.APIListen)
		if err != nil {
			return fmt.Errorf("api listen %s: %w", cfg.APIListen, err)
		}
		s := newHTTP()
		if tlsConf != nil {
			s.TLSConfig = tlsConf
			go func() { httpErr <- s.ServeTLS(l, "", "") }()
		} else {
			log.Warn("API TCP listener has no TLS; bearer tokens travel in clear text", "addr", cfg.APIListen)
			go func() { httpErr <- s.Serve(l) }()
		}
	}

	// ---- ready
	log.Info("horus started", "version", version, "dns", cfg.DNSListen, "api", cfg.APIListen,
		"socket", cfg.APISocket, "upstreams", cfg.Upstreams,
		"zones", holder.Load().Zones(), "records", holder.Load().Len())
	if _, err := sdnotify.Notify(sdnotify.Ready); err != nil {
		log.Warn("sd_notify failed", "err", err)
	}
	sdnotify.Status(fmt.Sprintf("serving %d zones, %d records", holder.Load().Zones(), holder.Load().Len()))
	go sdnotify.RunWatchdog(ctx, nil)

	// Periodic cache sweep.
	go func() {
		t := time.NewTicker(time.Minute)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				fwd.Cache().Sweep()
			}
		}
	}()

	// SIGHUP: reload snapshot from DB and flush the forwarding cache.
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	defer signal.Stop(hup)

	var runErr error
loop:
	for {
		select {
		case <-ctx.Done():
			break loop
		case <-hup:
			_, _ = sdnotify.Notify(sdnotify.Reloading)
			if err := reload(ctx); err != nil {
				log.Error("reload failed", "err", err)
			} else {
				fwd.Cache().Flush()
				log.Info("reloaded", "zones", holder.Load().Zones(), "records", holder.Load().Len())
			}
			_, _ = sdnotify.Notify(sdnotify.Ready)
		case err := <-dnsLn.Err():
			runErr = fmt.Errorf("dns server: %w", err)
			break loop
		case err := <-httpErr:
			if !errors.Is(err, http.ErrServerClosed) {
				runErr = fmt.Errorf("api server: %w", err)
				break loop
			}
		}
	}

	// ---- shutdown
	_, _ = sdnotify.Notify(sdnotify.Stopping)
	log.Info("shutting down")
	sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, s := range httpServers {
		_ = s.Shutdown(sctx)
	}
	_ = dnsLn.Shutdown(sctx)
	if cfg.APISocket != "" {
		_ = os.Remove(cfg.APISocket)
	}
	return runErr
}

func listenUnix(path string) (net.Listener, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return nil, err
	}
	// Remove a stale socket from a previous run.
	if fi, err := os.Lstat(path); err == nil && fi.Mode()&os.ModeSocket != 0 {
		_ = os.Remove(path)
	}
	l, err := net.Listen("unix", path)
	if err != nil {
		return nil, fmt.Errorf("api socket %s: %w", path, err)
	}
	if err := os.Chmod(path, 0o660); err != nil {
		l.Close()
		return nil, err
	}
	return l, nil
}

func newLogger(level string) *slog.Logger {
	var lv slog.Level
	_ = lv.UnmarshalText([]byte(level))
	return slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: lv}))
}
