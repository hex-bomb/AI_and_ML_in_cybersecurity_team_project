// Command retrieval — сервис гибридного поиска по базе знаний.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net"
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
	esIndex := env("ES_INDEX", "vektor-chunks-v2")
	embeddingURL := env("EMBEDDING_URL", "http://localhost:8000")
	addr := env("ADDR", ":8080")
	jwksURL := env("JWKS_URL", "http://localhost:8080/.well-known/jwks.json")

	// Один клиент на все внешние вызовы: общий пул соединений и таймаут.
	// DefaultTransport держит 2 idle-соединения на хост и при росте rps
	// пересоздаёт TCP на почти каждом запросе — отсюда TIME_WAIT и хвост latency.
	transport := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   2 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		MaxIdleConns:          200,
		MaxIdleConnsPerHost:   100,
		MaxConnsPerHost:       200,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   3 * time.Second,
		ExpectContinueTimeout: time.Second,
	}
	// Timeout клиента — верхняя страховка на вызов; рабочий дедлайн поиска задаёт handler.
	httpClient := &http.Client{Transport: transport, Timeout: 20 * time.Second}

	es := search.NewES(esURL, esIndex, httpClient)
	embedder := search.NewEmbedder(embeddingURL, httpClient)
	service := api.NewHybridSearch(es, embedder)
	verifier := jwks.NewVerifier(jwksURL)
	handler := api.NewHandler(service, verifier, api.NewMetrics())

	srv := &http.Server{
		Addr:    addr,
		Handler: handler.Routes(),
		// Без Read/Write/Idle таймаутов медленный клиент занимает соединение
		// бесконечно; WriteTimeout должен быть больше таймаута поиска.
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("server", "err", err)
			os.Exit(1)
		}
	}()
	<-ctx.Done()

	// До 10s на завершение активных запросов, дальше соединения рвутся.
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
