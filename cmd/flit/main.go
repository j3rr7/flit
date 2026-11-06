package main

import (
	"context"
	"errors"
	"flag"
	"flit"
	"flit/internal/hub"
	"flit/internal/server"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	addr := flag.String("addr", ":8080", "listen address")
	evict := flag.Duration("evict", 5*time.Minute, "idle room eviction TTL")
	flag.Parse()

	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	webFS, err := fs.Sub(flit.WebFS, "web")
	if err != nil {
		logger.Error("embed web/", "err", err)
		os.Exit(1)
	}

	if err := fs.WalkDir(webFS, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		logger.Info("embedded", "path", p, "dir", d.IsDir())
		return nil
	}); err != nil {
		logger.Error("walk embedded fs", "err", err)
		os.Exit(1)
	}

	h := hub.New(*evict, logger)
	handler := server.New(h, webFS, logger)

	srv := &http.Server{
		Addr:              *addr,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go func() {
		logger.Info("flit listening", "addr", *addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("server error", "err", err)
			stop()
		}
	}()

	<-ctx.Done()
	logger.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdownCtx)
}
