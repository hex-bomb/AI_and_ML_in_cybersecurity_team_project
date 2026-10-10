package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/kbsearch/backend-go/internal/app"
	"github.com/kbsearch/backend-go/internal/config"
	"github.com/kbsearch/backend-go/internal/migrations"
)

func main() {
	var (
		migrateOnly = flag.Bool("migrate", false, "применить миграции и выйти")
		healthcheck = flag.Bool("healthcheck", false, "проверить /health/live и выйти")
		addr        = flag.String("addr", "", "адрес слушателя поверх API_ADDR")
	)
	flag.Parse()

	if *healthcheck {
		os.Exit(runHealthcheck())
	}

	log.SetFlags(log.LstdFlags | log.LUTC)
	log.SetPrefix("api ")

	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("конфигурация: %v", err)
	}
	if *addr != "" {
		cfg.Addr = *addr
	}

	if *migrateOnly {
		if err := migrations.Up(cfg.DatabaseURL); err != nil {
			log.Fatalf("миграции: %v", err)
		}
		log.Println("схема актуальна")
		return
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	deps, err := app.New(ctx, cfg)
	if err != nil {
		log.Fatalf("сборка зависимостей: %v", err)
	}
	defer deps.Close()

	if err := migrations.Up(cfg.DatabaseURL); err != nil {
		log.Fatalf("миграции: %v", err)
	}

	server := &http.Server{
		Addr:              cfg.Addr,
		Handler:           deps.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       60 * time.Second,
		WriteTimeout:      120 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	go func() {
		<-ctx.Done()
		log.Println("остановка по сигналу")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			log.Printf("graceful shutdown: %v", err)
		}
	}()

	log.Printf("слушаю %s", cfg.Addr)
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("слушание: %v", err)
	}
	log.Println("остановлен")
}

// runHealthcheck probes /health/live using only API_ADDR so liveness does not
// depend on unrelated config like DATABASE_URL.
func runHealthcheck() int {
	addr := os.Getenv("API_ADDR")
	if addr == "" {
		addr = "127.0.0.1:8000"
	}
	client := &http.Client{Timeout: 4 * time.Second}
	resp, err := client.Get("http://" + addr + "/health/live")
	if err != nil {
		fmt.Fprintf(os.Stderr, "healthcheck: %v\n", err)
		return 1
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(os.Stderr, "healthcheck: HTTP %d\n", resp.StatusCode)
		return 1
	}
	return 0
}
