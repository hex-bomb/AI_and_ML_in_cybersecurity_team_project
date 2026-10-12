// Package rebuild пересобирает поисковый индекс из lake.
//
// §1, принцип 1: «источник правды один: сырой слой (lake) + события. Всё
// остальное (поисковый индекс, граф, PageRank, эмбеддинги) — производные
// представления, которые можно пересобрать из lake за часы». §4: «любой индекс
// можно удалить и воссоздать из lake без обращения к источникам». §9,
// «Восстановление»: «индекс хотя бы раз полностью пересобран из lake по учебной
// тревоге».
//
// Сценарий устроен так:
//
//	lake (ports.EventStore) → свёртка в Plan → нарезка → эмбеддер → индекс
//
// Чтения источников нет вообще: ни коннекторов, ни Kafka, ни object storage.
// Ровно это и проверяет §4 («без обращения к источникам»), и именно поэтому
// lake обязан нести весь документ — иначе пересборка собирала бы индекс по
// предположению.
//
// БЕЗОПАСНОСТЬ ПРЕРЫВАНИЯ (§4: «любой индекс можно удалить и воссоздать из
// lake»). Работа идёт по одному документу, и каждый документ переписывается по
// правилу «сначала снести, потом записать» — ровно как это делает стадия
// индексации (Indexer.writeChunks). Прерванная на середине пересборка даёт
// индекс, в котором часть документов новой ревизии, часть — прежней, и ни
// один не «наполовину записан»: запись одного документа атомарна с точки зрения
// пересборки, потому что чанки одного документа получают одни и те же
// детерминированные chunk_id и повторный Upsert перезаписывает их, а не
// плодит дубли. Повторный запуск безопасен и доводит дело до конца.
//
// ПОЧЕМУ ТУТ НЕТ ИМПОРТА internal/jobs/indexing. Нарезку на чанки делает
// indexing.Chunk, но сценарий не может импортировать соседний сценарий: по
// схеме развёртывания (layers_test.go) сценарии общаются только через порты и
// события, а прямая ссылка internal/jobs/rebuild → internal/jobs/indexing была
// бы новой записью в списке долгов. Поэтому нарезка объявлена здесь как порт
// Chunker (потребитель объявляет интерфейс, §8), а реализация — тонкая
// прослойка над настоящей нарезкой — собирается в точке входа (cmd/rebuild).
package rebuild

import (
	"context"
	"strings"

	"github.com/google/uuid"

	domain "github.com/kbsearch/kb/internal/envelope"
	"github.com/kbsearch/kb/internal/ports"
)

// IndexingSettings — единственный порт, который пересборке нужно от настроек.
//
// Узкий интерфейс, а не ports.SettingsRepository: пересборке нужен ровно
// профиль индекса для конкретной базы знаний, и требовать от неё весь реестр
// настроек означало бы запретить сборку без лишних прав.
type IndexingSettings interface {
	IndexingConfig(ctx context.Context, q ports.Queryer, tenantID, kbID string) (domain.IndexingConfig, error)
}

// Chunker нарезает восстановленный документ на чанки.
//
// КОНТРАКТ ПОРТА. Реализация возвращает чанки с ТЕКСТОМ, идентификатором,
// номером и локатором. Всё документное — права, заголовок, идентификаторы
// тенанта, базы, документа и версии, профиль вектора — проставляет сама
// пересборка (Service.chunks). Причина: это единственный документный контракт
// в этом сценарии, и раздать его двум авторам означало бы, что права в
// индексе и права восстановленного документа однажды разойдутся — а разойтись
// они могут только в одну сторону, в утечку (§6).
type Chunker interface {
	Chunk(doc domain.ParsedDocument, cfg domain.IndexingConfig) ([]domain.Chunk, error)
}

// Phase — этап пересборки в отчёте о ходе.
type Phase string

const (
	PhaseLake   Phase = "lake"
	PhaseRemove Phase = "remove"
	PhaseIndex  Phase = "index"
)

// Step — один шаг пересборки для наблюдателя.
type Step struct {
	Phase Phase
	// Done и Total — прогресс по документам внутри фазы.
	Done  int
	Total int
	// DocID — документ, над которым идёт работа. Пуст на чтении lake, где
	// документов ещё нет.
	DocID string
}

// ProgressFunc наблюдает ход пересборки.
//
// Обратный вызов, а не лог внутри сценария: сценарий ничего не знает о том,
// кто его запустил. nil означает «никто не смотрит» и стоит проверки на каждое
// обращение.
type ProgressFunc func(Step)

// Deps — порты пересборки индекса.
//
// Lake читается как EventStore и никогда как EventBus: пересборка — потребитель
// lake, и обратная запись в источник правды сделала бы восстановление
// зависимым от самого себя.
type Deps struct {
	Lake     ports.EventStore
	Settings IndexingSettings
	Queryer  ports.Queryer
	Chunk    Chunker
	Embedder ports.Embedder
	Index    ports.SearchIndex
	Progress ProgressFunc
}

// Scope — границы одной пересборки.
type Scope struct {
	// TenantID обязателен. Пересборка без тенанта прочитала бы чужие события:
	// порт EventStore это отвергает, но и сценарий не должен просить.
	TenantID uuid.UUID
	// KBID ограничивает пересборку одной базой знаний. Пусто — весь тенант.
	//
	// НАДГРОБИЯ ПРИ ЭТОМ ВСЕГДА ОБРАБАТЫВАЮТСЯ, даже когда KBID задан. Тело
	// надгробия не знает базы знаний (delete.go), и пропуск удалённого документа
	// из-за неизвестной базы означал бы ровно то нарушение §9, ради которого
	// пересборка и делается.
	KBID string
	// PageSize — размер страницы чтения lake. Ноль — PageSizeDefault.
	PageSize int
}

// Границы чтения lake. Верхняя совпадает с потолком адаптера (lake.go,
// maxLakeLimit): большая страница не даст ничего, кроме молчаливой обрезки.
const (
	PageSizeDefault = 500
	PageSizeMax     = 5000
)

// Validate проверяет границы пересборки.
func (s Scope) Validate() error {
	if s.TenantID == uuid.Nil {
		return domain.InvalidMetadata("Пересборка индекса без tenant_id: прочитала бы чужие события")
	}
	if s.PageSize < 0 {
		return domain.ConfigurationInvalid(
			"Размер страницы чтения lake отрицателен",
			map[string]any{"page_size": s.PageSize},
		)
	}
	if s.PageSize > PageSizeMax {
		return domain.ConfigurationInvalid(
			"Размер страницы чтения lake больше потолка адаптера",
			map[string]any{"page_size": s.PageSize, "max": PageSizeMax},
		)
	}
	// Идентификатор базы знаний проверяется ЗДЕСЬ, а не молча игнорируется при
	// сравнении. Опечатка в -kb иначе выглядела бы как «пересборка ничего не
	// нашла» и тихо оставила индекс прежним, а отчёт сказал бы «готово».
	if kb := s.KBFilter(); kb != "" {
		if parsed, err := uuid.Parse(kb); err != nil || parsed == uuid.Nil {
			return domain.ConfigurationInvalid(
				"Идентификатор базы знаний не разбирается",
				map[string]any{"kb_id": kb},
			)
		}
	}
	return nil
}

// KBFilter возвращает идентификатор базы знаний или пустую строку «весь тенант».
func (s Scope) KBFilter() string { return strings.TrimSpace(s.KBID) }

// Matches reports whether the document belongs to the scope.
func (s Scope) Matches(document Document) bool {
	kb := s.KBFilter()
	return kb == "" || document.KBID.String() == kb
}

// limit — фактический размер страницы чтения.
func (s Scope) limit() int {
	if s.PageSize <= 0 {
		return PageSizeDefault
	}
	return s.PageSize
}

// Report — итог одной пересборки.
//
// Числа различают «прочитано», «записано» и «снято» намеренно: отчёт пересборки
// читает человек после учебной тревоги, и «0 документов, 412 чанков» должно
// означать разные вещи в разных колонках.
type Report struct {
	// Events — событий lake прочитано.
	Events int
	// Documents — документов восстановлено из lake (все, до фильтра по базе).
	Documents int
	// Indexed — документов записано в индекс.
	Indexed int
	// Chunks — чанков записано.
	Chunks int
	// Tombstones — надгробий учтено.
	Tombstones int
	// RemovedChunks — чанков снято надгробиями.
	RemovedChunks int
	// Skipped — документов пропущено фильтром по базе знаний.
	Skipped int
	// Index — имя индекса, в который шла запись. Пусто, если нечего было писать.
	Index string
}

// Service пересобирает индекс одного тенанта из lake.
type Service struct {
	deps Deps
}

// New собирает пересборку.
//
// Частичная сборка запрещена и проверяется явно: сценарий без lake не имеет
// смысла, а сценарий без индекса или эмбеддера молча пересобрёт «индекс» в
// никуда. Проверка здесь, а не на первом обращении, чтобы обрыв пришёл на
// старте команды с понятным текстом, а не на середине многочасовой пересборки.
func New(deps Deps) (*Service, error) {
	missing := make([]string, 0, 5)
	for _, required := range []struct {
		name string
		ok   bool
	}{
		{"Lake", deps.Lake != nil},
		{"Settings", deps.Settings != nil},
		{"Queryer", deps.Queryer != nil},
		{"Chunk", deps.Chunk != nil},
		{"Embedder", deps.Embedder != nil},
		{"Index", deps.Index != nil},
	} {
		if !required.ok {
			missing = append(missing, required.name)
		}
	}
	if len(missing) > 0 {
		return nil, domain.ConfigurationInvalid(
			"Пересборка индекса собрана не полностью",
			map[string]any{"missing": missing},
		)
	}
	return &Service{deps: deps}, nil
}

// Rebuild пересобирает индекс из lake.
//
// Порядок шагов и его причины:
//
//  1. ПРОВЕРКА ЖИВОСТИ LAKE. Первым делом §9 требует, чтобы пересборка была
//     проверяемой, и первая проверка здесь — «lake вообще доступен». Падать на
//     этом дешевле и понятнее, чем после получаса чтения.
//  2. ЧТЕНИЕ И СВЁРТКА. Лента читается постранично и целиком: ленивая свёртка
//     по мере чтения означала бы, что прерванную пересборку нельзя повторить
//     «с того же места» — а повторный полный проход идемпотентен и стоит одного
//     SELECT'а.
//  3. НАДГРОБИЯ. Раньше записи и без эмбеддеров: удалённый документ не должен
//     оставаться в выдаче дольше, чем это необходимо.
//  4. ДОКУМЕНТЫ. Порядок документов задан планом (сортировка по doc_id), а не
//     порядком обхода карты.
func (s *Service) Rebuild(ctx context.Context, scope Scope) (Report, error) {
	if err := scope.Validate(); err != nil {
		return Report{}, err
	}
	if err := s.deps.Lake.Ping(ctx); err != nil {
		return Report{}, err
	}

	events, err := readLake(ctx, s.deps.Lake, scope)
	if err != nil {
		return Report{}, err
	}
	plan, err := Fold(events)
	if err != nil {
		return Report{}, err
	}

	report := Report{
		Events:     len(events),
		Documents:  len(plan.Documents),
		Tombstones: len(plan.Deleted),
	}
	s.step(Step{Phase: PhaseLake, Done: len(events), Total: len(events)})

	if err := s.removeTombstoned(ctx, scope, plan.Deleted, &report); err != nil {
		return report, err
	}
	if err := s.indexDocuments(ctx, scope, plan.Documents, &report); err != nil {
		return report, err
	}
	return report, nil
}

// removeTombstoned убирает из индекса документы, удалённые в источнике.
//
// §9: «документ, удалённый в источнике, исчезает из выдачи». Это свойство
// обязано выполняться и после пересборки: иначе lake знает об удалении, индекс
// — нет, и следующая пересборка вернула бы документ обратно.
//
// Удаление идёт по паре (tenant_id, document_id) — тем же ограничением, что и
// у стадии delete. Один document_id у двух тенантов — штатная ситуация (§3/§4).
func (s *Service) removeTombstoned(
	ctx context.Context, scope Scope, deleted []string, report *Report,
) error {
	tenant := scope.TenantID.String()
	for i, docID := range deleted {
		if err := ctx.Err(); err != nil {
			return interrupted(err, i, len(deleted))
		}
		s.step(Step{Phase: PhaseRemove, Done: i, Total: len(deleted), DocID: docID})
		removed, err := s.deps.Index.DeleteDocument(ctx, tenant, docID)
		if err != nil {
			return err
		}
		report.RemovedChunks += removed
	}
	s.step(Step{Phase: PhaseRemove, Done: len(deleted), Total: len(deleted)})
	return nil
}

// indexDocuments переиндексирует восстановленные документы.
//
// ensured — индексы уже созданные в этом прогоне, по profile_id. Проверка
// размерности вектора (EnsureIndex) на каждый документ стоила бы кругового
// обращения к mapping'у там, где профиль заведомо тот же.
func (s *Service) indexDocuments(
	ctx context.Context, scope Scope, documents []Document, report *Report,
) error {
	ensured := make(map[string]bool, len(documents))

	for i, document := range documents {
		if !scope.Matches(document) {
			report.Skipped++
			continue
		}
		if err := ctx.Err(); err != nil {
			return interrupted(err, i, len(documents))
		}
		s.step(Step{Phase: PhaseIndex, Done: i, Total: len(documents), DocID: document.ID()})

		cfg, err := s.deps.Settings.IndexingConfig(
			ctx, s.deps.Queryer, scope.TenantID.String(), document.KBID.String())
		if err != nil {
			return err
		}
		if report.Index == "" {
			report.Index = cfg.IndexName()
		}
		profile := cfg.ProfileID()
		if !ensured[profile] {
			if _, err := s.deps.Index.EnsureIndex(ctx, cfg); err != nil {
				return err
			}
			ensured[profile] = true
		}

		chunks, err := s.chunks(ctx, document, cfg)
		if err != nil {
			return err
		}
		// Сначала снести, потом записать — тем же порядком, что и у стадии
		// индексации: повтор с изменившимися настройками нарезки обязан унести
		// прежние чанки, а не оставить их рядом с новыми.
		if _, err := s.deps.Index.DeleteDocument(ctx, scope.TenantID.String(), document.ID()); err != nil {
			return err
		}
		written, err := s.deps.Index.UpsertChunks(ctx, cfg, chunks)
		if err != nil {
			return err
		}
		if written != len(chunks) {
			return domain.IndexUnavailable("В индекс записано меньше чанков, чем насчитано").
				WithDetails(map[string]any{
					"doc_id":   document.ID(),
					"expected": len(chunks),
					"actual":   written,
				})
		}
		report.Indexed++
		report.Chunks += written
	}
	s.step(Step{Phase: PhaseIndex, Done: len(documents), Total: len(documents)})
	return nil
}

// chunks нарезает документ, считает векторы и проставляет документный контракт.
func (s *Service) chunks(
	ctx context.Context, document Document, cfg domain.IndexingConfig,
) ([]domain.Chunk, error) {
	drafts, err := s.deps.Chunk.Chunk(document.Parsed(), cfg)
	if err != nil {
		return nil, err
	}
	if len(drafts) == 0 {
		// Молча пропустить здесь нельзя: документ был бы «восстановлен» и при
		// этом отсутствовал бы в выдаче, а отчёт об этом не сказал бы ничего.
		return nil, domain.InvalidMetadata(
			"Восстановленный документ не дал ни одного чанка: индекс собран не будет").
			WithDetails(map[string]any{"doc_id": document.ID()})
	}

	texts := make([]string, len(drafts))
	for i := range drafts {
		texts[i] = drafts[i].Text
	}
	vectors, err := s.deps.Embedder.Embed(ctx, texts, cfg)
	if err != nil {
		return nil, err
	}
	if len(vectors) != len(drafts) {
		return nil, domain.EmbeddingResponseInvalid("Число векторов не совпало с числом чанков").
			WithDetails(map[string]any{
				"doc_id":  document.ID(),
				"chunks":  len(drafts),
				"vectors": len(vectors),
			})
	}

	profile := cfg.ProfileID()
	chunks := make([]domain.Chunk, len(drafts))
	for i, draft := range drafts {
		chunks[i] = domain.Chunk{
			SchemaVersion:      ChunkSchemaVersion,
			ChunkID:            draft.ChunkID,
			TenantID:           document.TenantID.String(),
			KBID:               document.KBID.String(),
			DocumentID:         document.ID(),
			VersionID:          document.VersionID.String(),
			Rights:             document.Rights,
			ChunkNumber:        draft.ChunkNumber,
			Title:              document.Title,
			Text:               draft.Text,
			Locator:            draft.Locator,
			EmbeddingProfileID: profile,
			Embedding:          vectors[i],
		}
	}
	return chunks, nil
}

// step сообщает шаг наблюдателю, если он есть.
func (s *Service) step(step Step) {
	if s.deps.Progress == nil {
		return
	}
	s.deps.Progress(step)
}

// interrupted оформляет остановку по сигналу.
//
// Отмена контекста НЕ тихая: пересборка, прерванная на середине, оставляет индекс
// в промежуточном состоянии, и об этом обязано быть сказано вслух. Повторный
// запуск безопасен (см. документацию пакета), но «успешно завершилась» после
// отмены было бы враньём.
func interrupted(cause error, done, total int) error {
	return domain.Internal("Пересборка индекса прервана").
		WithDetails(map[string]any{"done": done, "total": total}).
		WithCause(cause)
}
