package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"time"

	"whatsapp-number-checker/internal/api"
	"whatsapp-number-checker/internal/config"
	"whatsapp-number-checker/internal/pool"
)

func main() {
	cfg := config.Load()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	if err := run(ctx, cfg); err != nil {
		slog.Error("No se pudo ejecutar la API", "error", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, cfg *config.Config) error {
	listener, err := net.Listen("tcp4", cfg.Addr)
	if err != nil {
		return fmt.Errorf("escuchar en %s (comprueba que no haya otra instancia): %w", cfg.Addr, err)
	}
	defer listener.Close()

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	// Inicializar el Pool multi-cuentas
	p := pool.New(ctx, cfg, slog.Default())
	if err := p.LoadSessions(ctx); err != nil {
		slog.Warn("Advertencia cargando sesiones previas", "error", err)
	}
	defer p.CloseAll()

	server := &http.Server{
		Handler:           api.New(p),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    8 * 1024,
		BaseContext:       func(net.Listener) context.Context { return ctx },
	}

	served := make(chan error, 1)
	go func() { served <- server.Serve(listener) }()

	baseURL := "http://" + cfg.Addr
	if cfg.Addr[0] == ':' {
		baseURL = "http://127.0.0.1" + cfg.Addr
	}

	slog.Info("Servidor iniciado con éxito",
		"dashboard", baseURL,
		"consultas_api", baseURL+"/check?number=NUMERO",
		"max_diario_por_chip", cfg.MaxDailyQueriesPerNumber,
		"meta_diaria_total", cfg.TargetDailyTotal,
		"carpeta_sesiones", cfg.SessionsDir,
	)

	var serveErr error
	select {
	case <-ctx.Done():
	case serveErr = <-served:
	}

	cancel()
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()

	if err := server.Shutdown(shutdownCtx); err != nil {
		_ = server.Close()
		return fmt.Errorf("cerrar servidor: %w", err)
	}

	if serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
		return serveErr
	}
	return nil
}
