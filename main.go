package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"syscall"
	"time"
)

func main() {
	cfg, err := LoadConfig()
	if err != nil {
		fmt.Fprintf(os.Stderr, "config error: %v\n", err)
		os.Exit(1)
	}

	initLogger(cfg.LogLevel)
	cfg.PrintStartupBanner()

	// Database
	store, err := NewStore(cfg)
	if err != nil {
		logError("database init failed: %v", err)
		os.Exit(1)
	}
	defer store.Close()

	// Account pool
	pool := NewPool(store, cfg)
	if err := pool.Start(); err != nil {
		logError("pool start failed: %v", err)
		os.Exit(1)
	}
	defer pool.Stop()

	// Handlers
	proxyHandler := NewProxyHandler(store, pool, cfg)
	adminHandler := NewAdminHandler(store, pool, cfg)
	startedAt := time.Now()

	// Router
	mux := http.NewServeMux()

	// Health check (no auth)
	mux.HandleFunc("GET /health", HealthHandler(store, pool, startedAt))

	// Admin routes (admin key auth)
	adminHandler.RegisterRoutes(mux, cfg.AdminKey)

	// API proxy routes (employee key auth -> account picker -> proxy)
	apiHandler := chainMiddleware(
		proxyHandler,
		AuthMiddleware(store),
		AccountPickerMiddleware(pool),
	)
	mux.Handle("/v1/", apiHandler)

	// Wrap everything with CORS and request logging
	handler := CORSMiddleware(RequestLogMiddleware(mux))

	srv := &http.Server{
		Addr:         fmt.Sprintf(":%d", cfg.Port),
		Handler:      handler,
		ReadTimeout:  cfg.ReadTimeout,
		WriteTimeout: cfg.WriteTimeout,
		IdleTimeout:  cfg.IdleTimeout,
	}

	// Background: log purge (daily, keep 90 days)
	go logPurgeLoop(store)

	// Graceful shutdown
	go func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
		sig := <-sigCh
		logInfo("received signal %v, shutting down...", sig)

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		if err := srv.Shutdown(ctx); err != nil {
			logError("shutdown error: %v", err)
		}
	}()

	logInfo("server ready on :%d (go %s, %s/%s)",
		cfg.Port, runtime.Version(), runtime.GOOS, runtime.GOARCH)

	if err := srv.ListenAndServe(); err != http.ErrServerClosed {
		logError("server error: %v", err)
		os.Exit(1)
	}

	logInfo("shutdown complete")
}

// chainMiddleware applies middlewares in order: first middleware is outermost.
func chainMiddleware(handler http.Handler, middlewares ...func(http.Handler) http.Handler) http.Handler {
	for i := len(middlewares) - 1; i >= 0; i-- {
		handler = middlewares[i](handler)
	}
	return handler
}

func logPurgeLoop(store *Store) {
	ticker := time.NewTicker(24 * time.Hour)
	defer ticker.Stop()

	for range ticker.C {
		deleted, err := store.PurgeOldLogs(90)
		if err != nil {
			logError("log purge: %v", err)
			continue
		}
		if deleted > 0 {
			logInfo("purged %d log entries older than 90 days", deleted)
		}
	}
}
