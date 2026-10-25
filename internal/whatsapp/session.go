package whatsapp

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"

	"github.com/mdp/qrterminal/v3"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
	_ "modernc.org/sqlite"
)

type Client struct {
	client    *whatsmeow.Client
	container *sqlstore.Container
	logger    *slog.Logger
	connected atomic.Bool
}

func Open(ctx context.Context, path string, logger *slog.Logger) (*Client, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("ruta de sesión: %w", err)
	}
	if err = os.MkdirAll(filepath.Dir(absolute), 0700); err != nil {
		return nil, fmt.Errorf("crear directorio de sesión: %w", err)
	}
	filePath := filepath.ToSlash(absolute)
	if !strings.HasPrefix(filePath, "/") {
		filePath = "/" + filePath // file:///D:/... on Windows.
	}
	dsn := &url.URL{Scheme: "file", Path: filePath}
	query := url.Values{"_pragma": {"foreign_keys(1)", "busy_timeout(5000)", "journal_mode(WAL)"}}
	dsn.RawQuery = query.Encode()
	db, err := sql.Open("sqlite", dsn.String())
	if err != nil {
		return nil, fmt.Errorf("abrir SQLite: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	container := sqlstore.NewWithDB(db, "sqlite", waLog.Noop)
	if err := container.Upgrade(ctx); err != nil {
		_ = container.Close()
		return nil, fmt.Errorf("preparar SQLite: %w", err)
	}
	device, err := container.GetFirstDevice(ctx)
	if err != nil {
		_ = container.Close()
		return nil, fmt.Errorf("recuperar sesión: %w", err)
	}
	client := whatsmeow.NewClient(device, waLog.Noop)
	client.EnableAutoReconnect = true
	client.InitialAutoReconnect = true
	client.BackgroundEventCtx = ctx
	c := &Client{client: client, container: container, logger: logger}
	client.AddEventHandler(c.handleEvent)
	return c, nil
}

func (c *Client) Ready() bool {
	return c.connected.Load() && c.client.IsConnected() && c.client.IsLoggedIn()
}

// Start begins pairing or restores a saved device. HTTP stays available while
// authentication is pending; checks receive 503 until the Connected event.
func (c *Client) Start(ctx context.Context, qrOutput io.Writer) error {
	if c.client.Store.ID == nil {
		qr, err := c.client.GetQRChannel(ctx)
		if err != nil {
			return fmt.Errorf("preparar QR: %w", err)
		}
		go func() {
			for item := range qr {
				switch item.Event {
				case whatsmeow.QRChannelEventCode:
					c.logger.Info("Escanea el QR en WhatsApp > Dispositivos vinculados > Vincular un dispositivo")
					qrterminal.GenerateHalfBlock(item.Code, qrterminal.L, qrOutput)
				case whatsmeow.QRChannelSuccess.Event:
					c.logger.Info("Cuenta vinculada; esperando conexión")
				default:
					if ctx.Err() == nil {
						c.logger.Warn("La vinculación no se completó; reinicia la API para obtener otro QR", "event", item.Event)
					}
				}
			}
		}()
	} else {
		c.logger.Info("Recuperando sesión guardada")
	}
	return c.client.ConnectContext(ctx)
}

func (c *Client) handleEvent(event any) {
	switch event.(type) {
	case *events.Connected:
		c.connected.Store(true)
		c.logger.Info("WhatsApp conectado; API lista para consultar")
	case *events.Disconnected, *events.KeepAliveTimeout:
		c.connected.Store(false)
		c.logger.Warn("Conexión con WhatsApp interrumpida; consultas temporalmente no disponibles")
	case *events.KeepAliveRestored:
		c.connected.Store(true)
		c.logger.Info("Conexión con WhatsApp recuperada")
	case *events.LoggedOut:
		c.connected.Store(false)
		c.logger.Error("Sesión revocada; reinicia la API y vuelve a vincular mediante QR")
	case events.PermanentDisconnect:
		c.connected.Store(false)
		c.logger.Error("WhatsApp cerró la conexión; revisa el dispositivo vinculado y reinicia la API", "event", fmt.Sprintf("%T", event))
	}
}

func (c *Client) Close() error {
	c.connected.Store(false)
	c.client.Disconnect()
	return c.container.Close()
}
