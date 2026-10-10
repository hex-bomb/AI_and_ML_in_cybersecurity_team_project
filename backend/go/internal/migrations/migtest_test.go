package migrations

import (
	"os"
	"testing"
)

func TestUpOnCleanDatabase(t *testing.T) {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL не задан")
	}
	if err := Up(url); err != nil {
		t.Fatalf("Up: %v", err)
	}
	if err := Up(url); err != nil {
		t.Fatalf("повторный Up: %v", err)
	}
	v, err := Version(url)
	if err != nil {
		t.Fatalf("Version: %v", err)
	}
	t.Logf("версия схемы после Up: %d", v)
	if v != 1 {
		t.Errorf("версия схемы %d, ожидалась 1", v)
	}
}
