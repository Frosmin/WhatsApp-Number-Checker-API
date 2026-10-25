package whatsapp

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"testing"

	"go.mau.fi/whatsmeow/proto/waAdv"
	"go.mau.fi/whatsmeow/types"
)

func TestSessionPersistsAcrossReopen(t *testing.T) {
	ctx := context.Background()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	path := filepath.Join(t.TempDir(), "dir with spaces #", "session.db")
	client, err := Open(ctx, path, logger)
	if err != nil {
		t.Fatal(err)
	}
	if client.Ready() {
		t.Fatal("unlinked session reported ready")
	}
	device := client.client.Store
	jid := types.NewJID("59170000000", types.DefaultUserServer)
	device.ID = &jid
	device.Account = &waAdv.ADVSignedDeviceIdentity{
		Details: []byte{1}, AccountSignature: make([]byte, 64),
		AccountSignatureKey: make([]byte, 32), DeviceSignature: make([]byte, 64),
	}
	if err := device.Save(ctx); err != nil {
		_ = client.Close()
		t.Fatal(err)
	}
	identity := append([]byte(nil), device.IdentityKey.Priv[:]...)
	if err := client.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(ctx, path, logger)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	if reopened.client.Store.ID == nil || *reopened.client.Store.ID != jid {
		t.Fatal("saved device lost")
	}
	if !bytes.Equal(identity, reopened.client.Store.IdentityKey.Priv[:]) {
		t.Fatal("identity regenerated on restart")
	}
	if reopened.Ready() {
		t.Fatal("saved but disconnected session reported ready")
	}
}

func TestDisconnectedCheckNeverReturnsSuccessfulFalse(t *testing.T) {
	c, err := Open(context.Background(), filepath.Join(t.TempDir(), "session.db"), slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	if _, err := c.Check(context.Background(), "+59170000000"); err == nil {
		t.Fatal("disconnected lookup returned a boolean")
	}
}
