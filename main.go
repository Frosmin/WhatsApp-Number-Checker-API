package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"time"

	"whatsapp-number-checker/internal/api"
	"whatsapp-number-checker/internal/whatsapp"
)

const address = "127.0.0.1:8080"

func main() {
	sessionPath := flag.String("session", "data/session.db", "archivo SQLite de sesión de WhatsApp")
	flag.Parse()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if err := run(ctx, *sessionPath); err != nil {
		slog.Error("No se pudo ejecutar la API", "error", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, sessionPath string) error {
	// Bind before connecting: a second process must not use the same session.
	listener, err := net.Listen("tcp4", address)
	if err != nil {
		return fmt.Errorf("escuchar en %s (comprueba que no haya otra instancia): %w", address, err)
	}
	defer listener.Close()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	client, err := whatsapp.Open(ctx, sessionPath, slog.Default())
	if err != nil {
		return err
	}
	defer client.Close()
	server := &http.Server{
		Handler:           api.New(client),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    8 * 1024,
		BaseContext:       func(net.Listener) context.Context { return ctx },
	}
	served := make(chan error, 1)
	go func() { served <- server.Serve(listener) }()
	started := make(chan struct{})
	go func() {
		defer close(started)
		if err := client.Start(ctx, os.Stdout); err != nil && ctx.Err() == nil {
			slog.Error("No se pudo conectar con WhatsApp; la API devolverá 503. Revisa internet y reinicia", "error", err)
		}
	}()
	slog.Info("API local disponible; las consultas devolverán 503 hasta conectar WhatsApp", "url", "http://"+address)
	var serveErr error
	select {
	case <-ctx.Done():
	case serveErr = <-served:
	}
	cancel()
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer shutdownCancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		_ = server.Close()
		return fmt.Errorf("cerrar servidor: %w", err)
	}
	<-started
	if serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
		return serveErr
	}
	return nil
}
