package config

import (
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	defaultAddr                 = "127.0.0.1:8000"
	defaultElasticsearchURL     = "http://localhost:59200"
	defaultFileStorageRoot      = "./var/files"
	defaultEmbeddingModelCache  = "./var/models"
	defaultEmbeddingModel       = "intfloat/multilingual-e5-small"
	defaultEmbeddingDimension   = 384
	defaultEmbeddingBatchSize   = 32
	defaultEmbeddingConcurrency = 2
	defaultLLMBaseURL           = "http://localhost:11434"
	defaultLLMModel             = "qwen3:4b"
	defaultLLMMaxToolRounds     = 2
	defaultLLMToolRoundTokens   = 256
	defaultLLMTemperature       = 0.75
	defaultAuthMode             = "local"
	defaultLocalTenantID        = "11111111-1111-4111-8111-111111111111"
	defaultMaxUploadBytes       = 20 * 1024 * 1024
	defaultWorkerPollInterval   = 2000
	// Источник №1 по схеме развёртывания — Markdown / HTML / TXT.
	// PDF и DOCX разбирает internal/ingest/parser.
	defaultAllowedUploadExts = ".md,.txt,.html,.docx,.pdf"
	maxLLMToolRounds         = 8
)

// Config is the process configuration, shared by API and worker.
type Config struct {
	Addr string

	DatabaseURL           string
	ElasticsearchURL      string
	ElasticsearchUser     string
	ElasticsearchPassword string

	FileStorageRoot     string
	EmbeddingModelCache string

	EmbeddingModel       string
	EmbeddingDimension   int
	EmbeddingBatchSize   int
	EmbeddingConcurrency int

	LLMBaseURL       string
	LLMModel         string
	LLMTemperature   float64
	LLMMaxToolRounds int
	LLMTokenBudget   int

	AuthMode      string
	LocalTenantID string

	CORSAllowedOrigins []string

	MaxUploadBytes    int64
	AllowedUploadExts []string

	WorkerPollInterval time.Duration
}

// Load collects configuration from the environment, reporting all problems at once.
func Load() (Config, error) {
	var problems []string

	parse := func(key string, fallback int) int {
		raw := strings.TrimSpace(os.Getenv(key))
		if raw == "" {
			return fallback
		}
		n, err := strconv.Atoi(raw)
		if err != nil {
			problems = append(problems, fmt.Sprintf("%s=%q не целое число", key, raw))
			return fallback
		}
		return n
	}
	parseInt64 := func(key string, fallback int64) int64 {
		raw := strings.TrimSpace(os.Getenv(key))
		if raw == "" {
			return fallback
		}
		n, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			problems = append(problems, fmt.Sprintf("%s=%q не целое число", key, raw))
			return fallback
		}
		return n
	}
	parseFloat := func(key string, fallback float64) float64 {
		raw := strings.TrimSpace(os.Getenv(key))
		if raw == "" {
			return fallback
		}
		f, err := strconv.ParseFloat(raw, 64)
		if err != nil {
			problems = append(problems, fmt.Sprintf("%s=%q не число", key, raw))
			return fallback
		}
		return f
	}

	c := Config{
		Addr:                  env("API_ADDR", defaultAddr),
		DatabaseURL:           os.Getenv("DATABASE_URL"),
		ElasticsearchURL:      env("ELASTICSEARCH_URL", defaultElasticsearchURL),
		ElasticsearchUser:     os.Getenv("ELASTICSEARCH_USERNAME"),
		ElasticsearchPassword: os.Getenv("ELASTICSEARCH_PASSWORD"),
		FileStorageRoot:       env("FILE_STORAGE_ROOT", defaultFileStorageRoot),
		EmbeddingModelCache:   env("EMBEDDING_MODEL_CACHE", defaultEmbeddingModelCache),
		EmbeddingModel:        env("EMBEDDING_MODEL", defaultEmbeddingModel),
		EmbeddingBatchSize:    parse("EMBEDDING_BATCH_SIZE", defaultEmbeddingBatchSize),
		EmbeddingConcurrency:  parse("EMBEDDING_CONCURRENCY", defaultEmbeddingConcurrency),
		LLMBaseURL:            env("LLM_PRIMARY_BASE_URL", defaultLLMBaseURL),
		LLMModel:              env("LLM_PRIMARY_MODEL", defaultLLMModel),
		LLMMaxToolRounds:      parse("LLM_MAX_TOOL_ROUNDS", defaultLLMMaxToolRounds),
		LLMTokenBudget:        parse("LLM_TOOL_ROUND_TOKENS", defaultLLMToolRoundTokens),
		AuthMode:              env("AUTH_MODE", defaultAuthMode),
		LocalTenantID:         env("LOCAL_TENANT_ID", defaultLocalTenantID),
		MaxUploadBytes:        parseInt64("MAX_UPLOAD_BYTES", defaultMaxUploadBytes),
		WorkerPollInterval:    time.Duration(parse("WORKER_POLL_INTERVAL_MS", defaultWorkerPollInterval)) * time.Millisecond,
	}
	c.EmbeddingDimension = parse("EMBEDDING_DIMENSION", defaultEmbeddingDimension)
	c.LLMTemperature = parseFloat("LLM_TEMPERATURE", defaultLLMTemperature)
	c.CORSAllowedOrigins = splitList(os.Getenv("CORS_ALLOWED_ORIGINS"))
	c.AllowedUploadExts = splitList(env("ALLOWED_UPLOAD_EXTENSIONS", defaultAllowedUploadExts))

	problems = append(problems, c.validate()...)
	if len(problems) > 0 {
		return c, fmt.Errorf("конфигурация некорректна: %s", strings.Join(problems, "; "))
	}
	return c, nil
}

func (c Config) validate() []string {
	var missing []string
	if c.DatabaseURL == "" {
		missing = append(missing, "DATABASE_URL")
	}
	if c.ElasticsearchURL == "" {
		missing = append(missing, "ELASTICSEARCH_URL")
	}
	if c.LLMBaseURL == "" {
		missing = append(missing, "LLM_PRIMARY_BASE_URL")
	}
	var wrong []string
	if c.EmbeddingDimension <= 0 {
		wrong = append(wrong, "EMBEDDING_DIMENSION должен быть положительным")
	}
	if c.EmbeddingBatchSize <= 0 {
		wrong = append(wrong, "EMBEDDING_BATCH_SIZE должен быть положительным")
	}
	if c.EmbeddingConcurrency <= 0 {
		wrong = append(wrong, "EMBEDDING_CONCURRENCY должен быть положительным")
	}
	if c.LLMTemperature < 0 || c.LLMTemperature > 2 {
		wrong = append(wrong, "LLM_TEMPERATURE должен быть в диапазоне 0..2")
	}
	if c.LLMMaxToolRounds < 0 || c.LLMMaxToolRounds > maxLLMToolRounds {
		wrong = append(wrong, "LLM_MAX_TOOL_ROUNDS должен быть в диапазоне 0..8")
	}
	if c.MaxUploadBytes <= 0 {
		wrong = append(wrong, "MAX_UPLOAD_BYTES должен быть положительным")
	}
	if c.AuthMode == "local" && c.LocalTenantID == "" {
		wrong = append(wrong, "LOCAL_TENANT_ID обязателен при AUTH_MODE=local")
	}
	if c.AuthMode == "local" && !isLocalListenAddr(c.Addr) {
		wrong = append(wrong, "AUTH_MODE=local запрещает конкретные внешние адреса (разрешены loopback и 0.0.0.0 под docker-публикацию на 127.0.0.1)")
	}
	if len(missing) > 0 {
		wrong = append(wrong, "не заданы переменные окружения: "+strings.Join(missing, ", "))
	}
	return wrong
}

func env(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

func splitList(raw string) []string {
	var out []string
	for _, part := range strings.Split(raw, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// isLocalListenAddr reports whether addr is safe for AUTH_MODE=local:
// loopback only, or unspecified (container listens everywhere, host mapping
// restricts exposure to 127.0.0.1).
func isLocalListenAddr(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	host = strings.ToLower(strings.Trim(host, "[]"))
	switch host {
	case "127.0.0.1", "::1", "localhost", "", "0.0.0.0", "::":
		return true
	}
	return false
}
