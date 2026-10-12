package config

import (
	"slices"
	"testing"
)

func baseEnv(t *testing.T) {
	t.Helper()
	t.Setenv("DATABASE_URL", "postgres://localhost:5432/kb?sslmode=disable")
	t.Setenv("ELASTICSEARCH_URL", "http://localhost:59200")
	t.Setenv("LLM_PRIMARY_BASE_URL", "http://localhost:11434")
	t.Setenv("AUTH_MODE", "local")
	t.Setenv("API_ADDR", "127.0.0.1:8000")
}

// TestLocalModeRequiresLoopback guards AUTH_MODE=local on public interfaces.
func TestLocalModeRequiresLoopback(t *testing.T) {
	baseEnv(t)
	for _, addr := range []string{"example.com:8000", "192.168.1.10:8000"} {
		t.Setenv("API_ADDR", addr)
		if _, err := Load(); err == nil {
			t.Errorf("ADDR=%s: ожидалась ошибка конфигурации", addr)
		}
	}
	for _, addr := range []string{"127.0.0.1:8000", "[::1]:8000", "localhost:8000", "0.0.0.0:8000", ":8000"} {
		t.Setenv("API_ADDR", addr)
		if _, err := Load(); err != nil {
			t.Errorf("ADDR=%s: отклонён: %v", addr, err)
		}
	}
}

// TestMaxUploadBytesParsesInt64 covers values beyond 32-bit range.
func TestMaxUploadBytesParsesInt64(t *testing.T) {
	baseEnv(t)
	t.Setenv("MAX_UPLOAD_BYTES", "5368709120")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.MaxUploadBytes != 5368709120 {
		t.Errorf("MaxUploadBytes = %d", cfg.MaxUploadBytes)
	}
	t.Setenv("MAX_UPLOAD_BYTES", "не число")
	if _, err := Load(); err == nil {
		t.Error("ожидалась ошибка на нечисловом MAX_UPLOAD_BYTES")
	}
}

// TestDefaultUploadExts excludes markdown.
func TestDefaultUploadExts(t *testing.T) {
	baseEnv(t)
	t.Setenv("ALLOWED_UPLOAD_EXTENSIONS", "")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if slices.Contains(cfg.AllowedUploadExts, ".markdown") {
		t.Errorf("дефолт содержит .markdown: %v", cfg.AllowedUploadExts)
	}
}
