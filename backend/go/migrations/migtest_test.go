package migrations

import (
	"os"
	"testing"
)

func TestUpOnCleanDatabase(t *testing.T) {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		// CI выставляет KB_GO_TEST_DATABASE_URL; читаем оба, иначе миграции
		// молча пропускались в CI и падали только локально.
		url = os.Getenv("KB_GO_TEST_DATABASE_URL")
	}
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
	// Ожидание зашито числом намеренно: забытое обновление после добавления
	// миграции обязано падать здесь, а не тихо расходиться с реальной схемой.
	if v != 3 {
		t.Errorf("версия схемы %d, ожидалась 3", v)
	}
}
