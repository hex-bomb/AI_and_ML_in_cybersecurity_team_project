package embeddings

import (
	"context"
	"errors"
	"fmt"

	"github.com/knights-analytics/hugot"
	"github.com/knights-analytics/hugot/backends"
	"github.com/knights-analytics/hugot/pipelines"
)

// newHugotSession создаёт ONNX-сессию hugot на чистом Go-бэкенде (без CGO;
// опции потоков/арены — только для ORT-бэкенда).
func newHugotSession(ctx context.Context, cfg Config) (Session, error) {
	session, err := hugot.NewGoSession(ctx)
	if err != nil {
		return nil, fmt.Errorf("сессия hugot: %w", err)
	}

	// OnnxFilePath обязателен: в репозитории несколько .onnx-кандидатов.
	opts := hugot.NewDownloadOptions()
	opts.OnnxFilePath = "onnx/model.onnx"

	modelPath, err := hugot.DownloadModel(ctx, cfg.Model, cfg.CacheDir, opts)
	if err != nil {
		return nil, errors.Join(fmt.Errorf("загрузка модели %q: %w", cfg.Model, err), session.Destroy())
	}

	pipeline, err := session.NewPipeline(backends.PipelineConfig[*pipelines.FeatureExtractionPipeline]{
		ModelPath:    modelPath,
		Name:         "embedder",
		ModelLoading: backends.ModelLoadingONNX,
	})
	if err != nil {
		return nil, errors.Join(fmt.Errorf("пайплайн hugot: %w", err), session.Destroy())
	}

	return &hugotPipeline{session: session, pipeline: pipeline}, nil
}

// hugotPipeline — адаптер к пайплайну hugot (отдельный тип ради Destroy).
type hugotPipeline struct {
	session  *hugot.Session
	pipeline *pipelines.FeatureExtractionPipeline
}

func (p *hugotPipeline) RunPipeline(ctx context.Context, texts []string) (*Result, error) {
	out, err := p.pipeline.RunPipeline(ctx, texts)
	if err != nil {
		return nil, err
	}
	return &Result{Embeddings: out.Embeddings}, nil
}

func (p *hugotPipeline) Destroy() error { return p.session.Destroy() }
