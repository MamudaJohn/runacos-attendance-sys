package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"cabv/internal/config"
	"cabv/internal/identity"
	"cabv/internal/resources"
	"cabv/internal/store"
	"cabv/internal/web"
	"github.com/jackc/pgx/v5/pgxpool"
)

func main() {
	if err := run(); err != nil {
		slog.Error("server stopped", "error", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	poolCfg, err := pgxpool.ParseConfig(cfg.DatabaseURL)
	if err != nil {
		return errors.New("invalid PostgreSQL configuration")
	}
	poolCfg.MaxConns = 5
	poolCfg.ConnConfig.ConnectTimeout = 3 * time.Second
	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		return errors.New("cannot initialize PostgreSQL pool")
	}
	defer pool.Close()
	files, err := resources.New(cfg.ResourceDir, cfg.ResourceQuotaBytes)
	if err != nil {
		return errors.New("cannot initialize private resource storage")
	}
	database := &store.Store{Pool: pool}
	go func() {
		ticker := time.NewTicker(time.Hour)
		defer ticker.Stop()
		for {
			maintenance, cancel := context.WithTimeout(ctx, 30*time.Second)
			if _, err := database.PruneAttendanceEvidence(maintenance); err != nil && ctx.Err() == nil {
				slog.Warn("cannot expire sensor observations", "error", err)
			}
			cancel()
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
	var phoneIdentity *identity.Service
	if cfg.PublicOrigin != "" {
		phoneIdentity, err = identity.New(database, cfg.WebAuthnRPID, cfg.PublicOrigin)
		if err != nil {
			return err
		}
	}
	server := &http.Server{Addr: cfg.HTTPAddr, Handler: web.Handler(pool.Ping, web.Options{Store: database, SecureCookies: cfg.SecureCookies, Files: files, Identity: phoneIdentity}), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 2 * time.Minute, WriteTimeout: 2 * time.Minute, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16 << 10}
	done := make(chan error, 1)
	go func() {
		slog.Info("server listening", "address", cfg.HTTPAddr, "tls", cfg.TLSCertFile != "", "identity_required", phoneIdentity != nil)
		if cfg.TLSCertFile != "" {
			done <- server.ListenAndServeTLS(cfg.TLSCertFile, cfg.TLSKeyFile)
		} else {
			done <- server.ListenAndServe()
		}
	}()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdown); err != nil {
			return err
		}
		err := <-done
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}
