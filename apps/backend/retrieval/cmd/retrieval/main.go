// Command retrieval — сервис гибридного поиска по базе знаний.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"vektor/retrieval/internal/api"
	"vektor/retrieval/internal/jwks"
	"vektor/retrieval/internal/search"
)

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))

	esURL := env("ES_URL", "http://localhost:9200")
	esIndex := env("ES_INDEX", "vektor-chunks")
	embeddingURL := env("EMBEDDING_URL", "http://localhost:8000")
	addr := env("ADDR", ":8080")
	jwksURL := env("JWKS_URL", "http://localhost:8080/.well-known/jwks.json")

	// Один клиент на все внешние вызовы: общий пул соединений и таймаут.
	httpClient := &http.Client{Timeout: 20 * time.Second}

	es := search.NewES(esURL, esIndex, httpClient)
	embedder := search.NewEmbedder(embeddingURL, httpClient)
	service := api.NewHybridSearch(es, embedder)
	verifier := jwks.NewVerifier(jwksURL)
	handler := api.NewHandler(service, verifier, api.NewMetrics())

	srv := &http.Server{Addr: addr, Handler: handler.Routes(), ReadHeaderTimeout: 5 * time.Second}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("server", "err", err)
			os.Exit(1)
		}
	}()
	<-ctx.Done()

	shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdown)
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
