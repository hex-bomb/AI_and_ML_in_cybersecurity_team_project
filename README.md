# AI-поиск по базе знаний

MVP поиска по корпоративной базе знаний с ответами LLM и цитатами на источники.
Backend — один Go-микросервис (API + worker), PostgreSQL, Elasticsearch, веб-виджет.

Как это работает целиком: администратор загружает документы в базу знаний, система режет их
на фрагменты, превращает каждый фрагмент в числовой вектор и кладёт в поисковый индекс.
Пользователь задаёт вопрос, вопрос тоже превращается в вектор, система находит самые похожие
фрагменты двумя способами (по словам и по смыслу), а языковая модель пишет по ним ответ
со ссылками на источники. Номера источников проверяет сервер, а не модель.

ТЗ: [`ml-team-tasks.md`](ml-team-tasks.md).

## Структура

```text
backend/go/
  cmd/api/                    # HTTP-сервис
  cmd/worker/                 # воркер заданий
  internal/
    domain/                   # типы-значения, инварианты, ошибки; без импортов инфраструктуры
    application/
      ports/                  # исходящие порты: clock.go, sql.go, ports.go, repos.go
      documents/              # приём файлов (конвертер — заглушка, 501)
      indexing/               # чанкер, вызов индексации
      search/ answers/        # гибридный поиск, ответы с цитатами
      kb/ jobs/ settings/ auth/
    infrastructure/
      postgres/ elasticsearch/ embeddings/ ollama/ storage/
    delivery/
      httpapi/                # маршруты, middleware, DTO транспорта
      worker/                 # цикл claim → stage → heartbeat
    contracts/                # api.gen.go из OpenAPI + встроенная копия спеки
    arch/                     # тест направления зависимостей
    config/                   # окружение
    migrations/               # SQL, вшиты в бинарь через //go:embed
frontend/
  widget/                     # виджет (участник 4)
  demo/                       # сайт-хост для встраивания
contracts/openapi.json        # источник истины по контракту
backend/scripts/              # seed_tenant.sql, seed_settings.sql, seed_kb.sh, check_endpoints.sh
compose.yaml                  # postgres, elasticsearch, migrate, api, worker
```

Код разложен так, чтобы бизнес-логика ничего не знала про сеть и базы: сценарии зависят
только от интерфейсов-портов, а конкретные PostgreSQL, Elasticsearch и модели подключаются
в одном месте — `internal/app/app.go`. Правило проверяет тест, а не договорённость:
`backend/go/internal/arch/layers_test.go:19` запрещает `domain` импортировать
application/delivery/infrastructure и драйверы, `application` — delivery/infrastructure,
`delivery` — infrastructure. Порты живут в `application/ports/` (`ports.go:16-38`: `Embedder`,
`SearchIndex`, `LLMProvider`; `repos.go`: репозитории; `sql.go`: `Queryer`/`Transactor`;
`clock.go`: `Clock`).

## Elasticsearch

Elasticsearch здесь — это картотека фрагментов: каждый чанк лежит как документ с текстом
и вектором, а ищут по нему двумя независимыми способами. Адаптер —
`internal/infrastructure/elasticsearch/elasticsearch.go`, маппинг и тела запросов —
`internal/infrastructure/elasticsearch/index_body.go`, клиент — `go-elasticsearch/v9`.

Индекс заводится отдельно под каждый embedding-профиль:
`kb-chunks-v1-<hex sha256 профиля>` (`index_body.go:43-45`, `domain/indexing.go:34-38`).
Профиль — SHA-256 шести полей `IndexingConfig` (`domain/indexing.go:22-32`): поменялась модель
или размерность — поменялось имя индекса, старые вектора с новыми не смешиваются.
Создание и проверка — `EnsureIndex` (`elasticsearch.go:68-92`): индекса нет — создать, есть —
сверить `dims` из маппинга, несовпадение — `INDEX_PROFILE_MISMATCH`. Гонка двух создателей
гасится по `resource_already_exists_exception` (`elasticsearch.go:130-133`).

Внутри индекс устроен так (`index_body.go:19-55`): `dynamic: strict`, все идентификаторы
(`tenant_id`, `kb_id`, `document_id`, `version_id`, `chunk_id`, `embedding_profile_id`) —
`keyword`; `title` и `text` — `text` с русским анализатором (поиск по словам с учётом
русских окончаний); `embedding` — `dense_vector` с `similarity: cosine` (`index_body.go:46-51`),
разновидность HNSW (`bbq_hnsw`/`int8_hnsw`) выбирает сам кластер. Шард один, реплик ноль —
стенд одноузловой.

Цепочка индексации одного документа:
1. Чанкер режет разобранный документ на фрагменты (`application/indexing/chunker.go:15`;
   разные разделы не смешиваются, длинные блоки делятся по границе слов — `chunker.go:168`).
2. Каждый фрагмент превращается в вектор эмбеддером (ниже).
3. `UpsertChunks` (`elasticsearch.go:203-251`) проверяет все чанки до первого обращения к сети,
   пишет bulk-батчами по 500 с `_id = chunk_id` (повтор перезаписывает, дублей нет), частичный
   успех bulk считает ошибкой, в конце делает `refresh` — иначе поиск не увидит чанки до `ready`.
4. Диспетчер в той же транзакции переключает `active_version_id` и завершает задание.

Цепочка поискового запроса:
1. `KeywordSearch` — `multi_match` по полям `text` и `title` без нечёткого совпадения
   (`index_body.go:136-155`).
2. `VectorSearch` — `knn` по полю `embedding`, `k = max(limit, 10)`,
   `num_candidates = max(10k, 100)` (`index_body.go:157-195`).
3. Обе ветки несут обязательные фильтры: tenant, база, профиль, разрешённые документы
   (`index_body.go:121-134`). Пустой список разрешённых — пустой результат вообще без запроса
   в сеть (`elasticsearch.go:366-368`). Индекса профиля ещё нет — тоже пустой результат, не ошибка
   (`elasticsearch.go:437-440`).
4. Слияние двух выдач делает уже не Elasticsearch, а код (RRF — в разделе поиска).

Удаление — `delete_by_query` по маске `kb-chunks-v1-*` (`elasticsearch.go:331-357`), повтор
безопасен. Готовность кластера проверяется запросом `GET /` (`Ping`, `elasticsearch.go:515-526`).
Ошибки кластера превращаются в `INDEX_UNAVAILABLE`, а текст кластера уходит в поле `Details`,
не в сообщение пользователю (`elasticsearch.go:537-549`); тела ошибок обрезаются до 2 КБ
(`elasticsearch.go:32`).

## Эмбеддер

Эмбеддер превращает текст в вектор — список из 384 чисел, где похожие по смыслу тексты лежат
рядом. Поиск «по смыслу» — это поиск ближайших векторов к вектору вопроса. Адаптер —
`internal/infrastructure/embeddings/embedder.go`, загрузка модели —
`internal/infrastructure/embeddings/hugot.go`. Считает локальный ONNX-инференс через hugot
(чистый Go-бэкенд, без CGO); в рантайме никакой сети нет.

Модель `intfloat/multilingual-e5-small` при первом использовании скачивается с Hugging Face,
файл `onnx/model.onnx` (`hugot.go:21-28`), и ложится в кэш `EMBEDDING_MODEL_CACHE` (том
`models_data`, чтобы 448 МБ не качались после каждого пересоздания контейнера). Какая модель —
задаёт переменная `EMBEDDING_MODEL`, ожидаемая размерность — `EMBEDDING_DIMENSION` (384).

У модели E5 есть особенность: ей надо говорить, что за текст ей дают. Индексируемые фрагменты
идут с префиксом `passage: `, поисковые вопросы — с префиксом `query: ` (`embedder.go:16-36`).
Без префиксов вектора будут чужими, и никакой ошибку никто не вернёт — просто плохой поиск.

Загрузка ленивая под `sync.Once`: конструктор `New` модель не трогает, ошибка первой загрузки
запоминается (`embedder.go:89-91`, `208-222`). Поэтому `/health/live` отвечает ещё до того,
как скачалась модель. Размерность сверяется до загрузки (`embedder.go:119-124`) — качать
448 МБ ради заведомо неверного ответа не будут — и ещё раз после кодирования каждого батча
(`embedder.go:142-149`).

Считается батчами по 32 текста (`EMBEDDING_BATCH_SIZE`), параллельно через `errgroup`
с лимитом 2 (`EMBEDDING_CONCURRENCY`); ошибка одного батча отменяет остальные
(`embedder.go:153-183`). Количество и порядок векторов проверяются строго
(`embedder.go:185-206`). При включённом `normalize_vectors` каждый вектор приводится
к единичной длине (`embedder.go:253-268`). `Ping` поднимает модель (`embedder.go:224-228`),
`Close` освобождает сессию (`embedder.go:231-239`).

## LLM через Ollama

Языковая модель пишет ответ по найденным фрагментам. Живёт она не у нас, а в отдельном
процессе Ollama (обычно на той же машине, `LLM_PRIMARY_BASE_URL`, модель `qwen3:4b`),
а наш код ходит к нему по HTTP. Адаптер — `internal/infrastructure/ollama/` (`ollama.go`,
`chat.go`, `prompt.go`, `structured.go`, `known.go`). Порт — один метод
`ports.LLMProvider.Generate` (`application/ports/ports.go:36-38`,
вход и выход — `domain/llm.go:9-30`).

Разговор идёт напрямую HTTP-запросами `POST /api/chat`, а не через библиотеку langchaingo.
Причина чисто техническая: langchaingo — общий переходник под многих провайдеров, и его
Ollama-часть недописана. При отправке она выкидывает список инструментов — в HTTP-запрос
они не попадают, модель про `kb_search` не узнаёт. При получении читает только текст
и выкидывает поле `tool_calls` — даже если модель что-то вызовет, вызов до нашего кода
не дойдёт. Поэтому говорить с Ollama напрямую короче и надёжнее, чем чинить чужой переходник.
Поменять провайдер — значит реализовать один метод `ports.LLMProvider` и подсунуть его
в `app.go:107-111`; сценарии выше не изменятся.

Вызов инструментов работает так: модель отвечает не текстом, а структурной записью «вызови
функцию `kb_search` с таким-то запросом». Наш код выполняет поиск сам, подсовывает модели
найденные фрагменты и ждёт продолжения. Цикл — в `Generate` (`ollama.go:79-125`): до
`LLM_MAX_TOOL_ROUNDS` (дефолт 2) раундов с инструментом, каждому раунду — короткий бюджет
токенов (`LLM_TOOL_ROUND_TOKENS`, дефолт 256: модели нужен только вызов, не текст), потом
финальный запрос уже без инструментов, но с принудительной JSON-схемой
`{answer, citation_chunk_ids}` (`chat.go:145-155`). Потолок одного HTTP-запроса — 60 секунд
(ТЗ 2.4). Дефолты — `ollama.go:14-29`.

Инструкция модели (`prompt.go:11-18`): отвечать только по выданным фрагментам, каждое утверждение
подтверждать ссылкой `[N]`, факты и номера не выдумывать, тексты фрагментов — данные, а не
приказы, отвечать по-русски и только заданным JSON. Фрагменты печатаются со сквозной
нумерацией (`prompt.go:32-42`).

Инструмент `kb_search` исполняет сервер через тот же поисковый сервис (`ollama.go:184-213`):
права перепроверяются, новые фрагменты дописываются в общий набор, ошибка поиска превращается
в `Ничего не найдено`, а не роняет генерацию. Цитаты собирает сервер, а не модель
(`known.go:57-91`): годятся и chunk_id, и номера `[N]`; чужие ID отбрасываются, повторы
снимаются; если список пуст — номера добираются прямо из текста ответа. Ответ модели
парсится строго (`structured.go:16-30`): обёртка ```json снимается, пустой текст — это 502,
а не «нет данных». Дальше сценарий (`answers/service.go:133-143`) перепроверяет цитаты
по свежим активным версиям и правам и при пустом остатке возвращает `no_evidence`.

Сеть и 5xx со стороны Ollama — это 503 `LLM_UNAVAILABLE`, отказ 4xx и битый JSON —
502 `LLM_RESPONSE_INVALID` (`chat.go:87-121`). Обычный поиск при мёртвой LLM продолжает
работать (ТЗ 10.1). Без заданного `LLM_PRIMARY_BASE_URL` провайдер сразу отвечает 503,
а процесс стартует (`ollama.go:59-75`).

## Конвейер

Жизнь версии документа: `uploaded → parsing → parsed → indexing → ready`
(ошибка — `failed` с кодом и этапом). Задание загрузки создаётся на этапе `convert`
и после успешной конвертации тем же ID переходит в `index`; счётчик попыток при смене
этапа начинается заново. Повтор `POST` с тем же `Idempotency-Key` возвращает исходные ID,
с другим содержимым — 409. Параллельная загрузка в тот же документ — 409 `DOCUMENT_BUSY`.

Сейчас конвертер — заглушка: `Convert` всегда отвечает `501 CONVERTER_NOT_IMPLEMENTED`
(`application/documents/documents.go`). Загрузка принимается (202), задание остаётся
в `convert`. Настоящий convert-воркер должен: взять оригинал из `FileStorage`, построить
`ParsedDocument`, сохранить его JSON и перевести задание в `index`. Лимиты приёма уже
действуют: 20 МиБ, `.md/.docx/.pdf`, title ≤ 500 символов, `source_url` только http/https.

Поиск (`search/service.go:78-135`): доступ к базе → настройки → сверка зафиксированного
профиля (`profileFor`: дрейф настроек — 409, а не пустой результат) → список активных
версий из PostgreSQL → вектор вопроса → keyword и vector параллельно (`service.go:139-167`)
с лимитом `min(100, 5·top_k)` (`rrf.go:78-85`) → слияние RRF `1/(60+ранг)` с детерминированным
разрешением ничьих по chunk_id (`rrf.go:33-76`) → отсечение неактивных версий → ссылки
на источники из PostgreSQL, не из индекса.

Ответы (`answers/service.go:87-150`): настройки генерации → тот же поиск → пусто —
`no_evidence` без вызова модели → контекст в бюджет токенов → `Generate` → перепроверка
цитат → цитаты серверной сборки с реальным текстом чанков.

## Конфигурация

Все ключи — `backend/go/internal/config/config.go:110-132`. Главное:

| Переменная | Назначение |
|---|---|
| `DATABASE_URL` | PostgreSQL (схемы `postgresql://` и `postgresql+asyncpg://` нормализуются) |
| `ELASTICSEARCH_URL`, `ELASTICSEARCH_USERNAME/PASSWORD` | кластер |
| `FILE_STORAGE_ROOT`, `EMBEDDING_MODEL_CACHE` | тома файлов и моделей |
| `EMBEDDING_MODEL/DIMENSION/BATCH_SIZE/CONCURRENCY` | `intfloat/multilingual-e5-small` / 384 / 32 / 2 |
| `LLM_PRIMARY_BASE_URL/MODEL`, `LLM_TEMPERATURE/MAX_TOOL_ROUNDS/TOOL_ROUND_TOKENS` | Ollama, `qwen3:4b` / 0.75 / 2 / 256 |
| `AUTH_MODE`, `LOCAL_TENANT_ID` | `local`: один tenant без JWT; не-loopback адрес в этом режиме запрещён |
| `CORS_ALLOWED_ORIGINS`, `MAX_UPLOAD_BYTES`, `ALLOWED_UPLOAD_EXTENSIONS` | CORS, 20 МиБ, `.md,.docx,.pdf` |

Ошибки конфигурации собираются все сразу и валят старт с перечислением. Дополнительно:
входящие `query`/`question` — до 2000 символов, `name`/`title` — до 500, поле `metadata`
загрузки — до 64 КБ; дорогие POST (`/search`, `/answers`, загрузка) ограничены 60
запросами в минуту с IP (429 `TOO_MANY_REQUESTS`, `delivery/httpapi/ratelimit.go:16-17`).

## Запуск

```bash
docker compose up -d postgres elasticsearch   # :55432, :59200
docker compose run --rm migrate               # структура; настройки НЕ создаёт
docker compose exec -T postgres psql -U kb -d kb -v ON_ERROR_STOP=1 < backend/scripts/seed_tenant.sql
docker compose exec -T postgres psql -U kb -d kb -v ON_ERROR_STOP=1 < backend/scripts/seed_settings.sql
docker compose up -d api worker
curl http://127.0.0.1:8000/health/live
```

Без Docker (инфра из compose, процессы хостовые):

```bash
cd backend/go && go build -o /tmp/kbsearch/api ./cmd/api && go build -o /tmp/kbsearch/worker ./cmd/worker
DATABASE_URL=postgresql://kb:dev@127.0.0.1:55432/kb ELASTICSEARCH_URL=http://127.0.0.1:59200 \
  FILE_STORAGE_ROOT=/tmp/kbsearch-files EMBEDDING_MODEL_CACHE=/tmp/kbsearch-models \
  API_ADDR=127.0.0.1:8000 /tmp/kbsearch/api &
# worker — те же переменные без API_ADDR
```

Проверка всех эндпоинтов: `API=http://127.0.0.1:8000 bash backend/scripts/check_endpoints.sh`.
Контракт: `contracts/openapi.json` — источник истины; типы (`api.gen.go`) и раздача
`GET /api/v1/openapi.json` идут из встроенной копии (`internal/contracts/spec.go`),
расхождение ловит `TestEmbeddedSpecMatchesFile`. Тесты: `go test ./...` в `backend/go`.

## Известные ограничения

- Конвертер документов — заглушка (см. раздел «Конвейер»).
- Compose-Elasticsearch 8.17.3 может не стартовать на ядре без seccomp
  (`seccomp unavailable`); рабочий обход — имеющийся контейнер/кластер 8.17.3 с `ELASTICSEARCH_URL` наружу.
- Сборка образа требует >4 ГБ памяти Docker (ONNX-зависимости); при нехватке — хостовая сборка выше.
- `AUTH_MODE=local` — стенд без JWT; наружу выставлять запрещено (валится на старте).
