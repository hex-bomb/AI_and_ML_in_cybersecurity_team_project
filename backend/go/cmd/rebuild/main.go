// Команда пересборки индекса из lake — §9, «Восстановление»: «индекс хотя бы раз
// полностью пересобран из lake по учебной тревоге».
//
// ЭТО ОПЕРАЦИОННЫЙ ПРОГОН, А НЕ ЧАСТЬ РАБОЧЕГО ЦИКЛА. Пересборка не берёт
// задание из очереди и не ждёт сигнала: её запускает человек (или учебная
// тревога) и ждёт ответа. Поэтому это отдельная команда, как -migrate у
// cmd/api, а не стадия worker.Stage.
//
// ПОЧЕМУ ТАК ПРИ ЧЕМ ОДИН СОСТАВ ЗАВИСИМОСТЕЙ. Сборка повторяет приёмы
// cmd/worker (тот же config.Load, тот же пул, тот же адаптер индекса), но
// пересборка объявляет ровно те порты, которые ей нужны, и ничего лишнего: ни
// сценариев HTTP, ни очереди заданий, ни авторизации запросов. Сценарий
// rebuild.New проверяет полноту состава сам и падает на старте с перечнем
// недостающих портов, а не на середине многочасовой пересборки.
//
// ЧТО ПРОГОН ДЕЛАЕТ ПО ШАГАМ:
//
//	проверяет живость lake → читает ленту тенанта постранично → сворачивает её в
//	план → убирает из индекса документы, удалённые надгробиями → нарезает
//	оставшиеся, считает векторы и перезаписывает их чанки.
//
// Код возврата ненулевой при любой ошибке — прогон, не сделавший свою работу,
// обязан выглядеть провалившимся и в CI, и в учебной тревоге.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/google/uuid"

	"github.com/kbsearch/kb/internal/docs/settings"
	domain "github.com/kbsearch/kb/internal/envelope"
	"github.com/kbsearch/kb/internal/jobs/indexing"
	"github.com/kbsearch/kb/internal/jobs/rebuild"
	"github.com/kbsearch/kb/internal/platform/config"
	elasticsearch "github.com/kbsearch/kb/internal/store/es"
	postgres "github.com/kbsearch/kb/internal/store/pg"
	embeddings "github.com/kbsearch/kb/internal/store/tei"
)

func main() {
	os.Exit(run())
}

// run держит всю работу и весь код возврата в одной функции: так весь путь
// «запустился → отработал → упал» виден сверху вниз, и в журнале не теряется ни
// одна ошибка. main только отдаёт код возврата.
func run() int {
	var (
		tenantFlag = flag.String("tenant", "", "tenant_id, чей индекс пересобирается (обязателен)")
		kbFlag     = flag.String("kb", "", "kb_id; пусто — весь тенант")
		pageFlag   = flag.Int("page", rebuild.PageSizeDefault, "размер страницы чтения lake")
		quiet      = flag.Bool("quiet", false, "не печатать ход пересборки")
		timeout    = flag.Duration("timeout", 0, "ограничение по времени; 0 — без ограничения")
	)
	flag.Parse()

	log.SetFlags(log.LstdFlags | log.LUTC)
	log.SetPrefix("rebuild ")

	tenant, err := parseTenant(*tenantFlag)
	if err != nil {
		log.Printf("аргументы: %v", err)
		return 2
	}

	cfg, err := config.Load()
	if err != nil {
		log.Printf("конфигурация: %v", err)
		return 1
	}

	// Прерывание по Ctrl-C обязано быть громким: индекс после прерывания
	// неполон, и тихо выйти с кодом 0 значило бы отчитаться успехом там, где
	// пересборка не закончилась.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if *timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, *timeout)
		defer cancel()
	}

	service, closeDeps, err := build(ctx, cfg, *quiet)
	if err != nil {
		log.Printf("сборка зависимостей: %v", err)
		return 1
	}
	defer closeDeps()

	scope := rebuild.Scope{
		TenantID: tenant,
		KBID:     strings.TrimSpace(*kbFlag),
		PageSize: *pageFlag,
	}
	if err := scope.Validate(); err != nil {
		log.Printf("область пересборки: %v", err)
		return 2
	}

	log.Printf("пересборка индекса: tenant=%s kb=%s", tenant, orAll(scope.KBID))
	started := time.Now()
	report, err := service.Rebuild(ctx, scope)
	if err != nil {
		log.Printf("пересборка не завершена: %v (прочитано событий: %d)", err, report.Events)
		return 1
	}

	log.Printf("готово за %s: индекс %s", time.Since(started).Round(time.Millisecond),
		orNone(report.Index))
	log.Printf("  событий lake:    %d", report.Events)
	log.Printf("  документов:      %d восстановлено, %d записано, %d пропущено фильтром базы",
		report.Documents, report.Indexed, report.Skipped)
	log.Printf("  чанков:          %d записано", report.Chunks)
	log.Printf("  надгробий:       %d учтено, %d чанков убрано",
		report.Tombstones, report.RemovedChunks)
	return 0
}

// build собирает пересборку над настоящими адаптерами.
//
// Порядок освобождения задан здесь же, а не в сценарии: пул и кластер
// принадлежат команде, и закрывает их тот, кто открыл. Сценарий их только
// использует.
func build(ctx context.Context, cfg config.Config, quiet bool) (*rebuild.Service, func(), error) {
	pool, err := postgres.New(ctx, postgres.Config{URL: cfg.DatabaseURL})
	if err != nil {
		return nil, nil, fmt.Errorf("подключение к PostgreSQL: %w", err)
	}

	index, err := elasticsearch.New(elasticsearch.Config{
		Addresses: []string{cfg.ElasticsearchURL},
		Username:  cfg.ElasticsearchUser,
		Password:  cfg.ElasticsearchPassword,
	})
	if err != nil {
		pool.Close()
		return nil, nil, fmt.Errorf("хранилище поиска: %w", err)
	}

	// Модель грузится лениво (первый вызов Embed), поэтому холодная пересборка
	// стартует мгновенно и платит за модель на первом документе.
	embedder := embeddings.New(embeddings.Config{
		Model:       cfg.EmbeddingModel,
		Dimension:   cfg.EmbeddingDimension,
		CacheDir:    cfg.EmbeddingModelCache,
		BatchSize:   cfg.EmbeddingBatchSize,
		Concurrency: cfg.EmbeddingConcurrency,
	})

	service, err := rebuild.New(rebuild.Deps{
		// Один экземпляр lake на оба шва: читает пересборка, пишет стадия
		// delete в воркере. Идемпотентность lake держится на реестре
		// raw_event_ids внутри Publish, и второй писатель обошёл бы его.
		Lake:     postgres.NewLake(pool),
		Settings: settings.New(postgres.NewSettingsRepository()),
		Queryer:  pool,
		Chunk:    indexerChunker{},
		Embedder: embedder,
		Index:    index,
		Progress: newProgressPrinter(quiet),
	})
	if err != nil {
		_ = embedder.Close()
		_ = index.Close(context.Background())
		pool.Close()
		return nil, nil, err
	}

	return service, func() {
		_ = embedder.Close()
		_ = index.Close(context.Background())
		pool.Close()
	}, nil
}

// indexerChunker — реализация rebuild.Chunker поверх настоящей нарезки
// internal/jobs/indexing.
//
// Сценарий не может импортировать соседний сценарий (layers_test.go: сценарии
// общаются только через порты и события), поэтому нарезка объявлена портом, а
// эта прослойка — её единственная реализация. Дублировать алгоритм нарезки в
// пересборке было бы худшим из решений: две нарезки разошлись бы при первой же
// правке параметров, и пересобранный индекс отличался бы от индекса конвейера.
//
// Документные поля (права, заголовок, идентификаторы, профиль) НЕ проставляются
// здесь: это делает сама пересборка, и она же владеет этим контрактом.
type indexerChunker struct{}

var _ rebuild.Chunker = indexerChunker{}

func (indexerChunker) Chunk(
	doc domain.ParsedDocument, cfg domain.IndexingConfig,
) ([]domain.Chunk, error) {
	drafts, err := indexing.Chunk(doc, cfg)
	if err != nil {
		return nil, err
	}
	out := make([]domain.Chunk, len(drafts))
	for i, draft := range drafts {
		out[i] = domain.Chunk{
			ChunkID:     draft.ChunkID,
			ChunkNumber: draft.Number,
			Text:        draft.Text,
			Locator: domain.Locator{
				BlockIDs:    draft.BlockIDs,
				SectionPath: draft.SectionPath,
				PageStart:   draft.PageStart,
				PageEnd:     draft.PageEnd,
			},
		}
	}
	return out, nil
}

// progressPrinter печатает ход пересборки.
//
// Не чаще раза в logStep документов: пересборка большого тенанта даёт
// десятки тысяч документов, а строка на каждый документ превратила бы журнал
// в мусор, из которого нельзя вычленить ни одной полезной строки.
const logStep = 100

func newProgressPrinter(quiet bool) rebuild.ProgressFunc {
	if quiet {
		return nil
	}
	logged := map[rebuild.Phase]int{}
	return func(step rebuild.Step) {
		if step.Total == 0 {
			return
		}
		last := logged[step.Phase]
		if step.Done != 0 && step.Done-last < logStep && step.Done != step.Total {
			return
		}
		logged[step.Phase] = step.Done
		switch step.Phase {
		case rebuild.PhaseLake:
			log.Printf("  прочитано событий lake: %d", step.Done)
		case rebuild.PhaseRemove:
			log.Printf("  убрано по надгробиям: %d из %d (%s)", step.Done, step.Total, step.DocID)
		case rebuild.PhaseIndex:
			log.Printf("  переиндексировано: %d из %d (%s)", step.Done, step.Total, step.DocID)
		}
	}
}

// parseTenant разбирает обязательный tenant_id.
func parseTenant(raw string) (uuid.UUID, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return uuid.Nil, errors.New("обязателен -tenant с идентификатором организации; " +
			"пересборка без тенанта прочитала бы чужие события")
	}
	parsed, err := uuid.Parse(trimmed)
	if err != nil || parsed == uuid.Nil {
		return uuid.Nil, fmt.Errorf("-tenant %q не является идентификатором организации", raw)
	}
	return parsed, nil
}

func orAll(kbID string) string {
	if kbID == "" {
		return "весь тенант"
	}
	return kbID
}

func orNone(name string) string {
	if name == "" {
		return "(не создавался: записывать было нечего)"
	}
	return name
}
