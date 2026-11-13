package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadDotEnv(t *testing.T) {
	tempDir := t.TempDir()
	envPath := filepath.Join(tempDir, ".env")

	content := `
# Archivo de prueba
MAX_DAILY_QUERIES_PER_NUMBER = "2500"
TARGET_DAILY_TOTAL = '15000'
REQUEST_DELAY_MS = 250
`
	if err := os.WriteFile(envPath, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	loadDotEnv(envPath)

	if val := os.Getenv("MAX_DAILY_QUERIES_PER_NUMBER"); val != "2500" {
		t.Fatalf("expected 2500, got %s", val)
	}
	if val := os.Getenv("TARGET_DAILY_TOTAL"); val != "15000" {
		t.Fatalf("expected 15000, got %s", val)
	}
	if val := os.Getenv("REQUEST_DELAY_MS"); val != "250" {
		t.Fatalf("expected 250, got %s", val)
	}
}
