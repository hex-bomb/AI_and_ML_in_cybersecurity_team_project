# Техническое задание: AI-поиск по базе знаний

Версия документа: 2.3. Дата: 09.10.2026.

Основание: схема `ml.drawio`. Исходный выбор Python для backend отменён в редакции 2.3, см. историю изменений. Это задания на реализацию MVP. Приведенные контракты обязательны для четырех участников; изменение контракта требует обновить этот документ, схемы и тестовые примеры потребителей.

### История изменений

| Версия | Дата | Что изменилось |
|---|---|---|
| 2.3 | 09.10.2026 | **Python удалён из проекта полностью.** Backend — один Go-микросервис: HTTP, работа с документами, индексация, эмбеддинги, поиск, ответы и worker. Версионирование схемы перешло с Alembic на golang-migrate, SQL-миграции вшиты в бинарь. Редакция 2.2 (Go + Python на границе по документам) отменена как промежуточная |
| 2.1 | 09.10.2026 | ~~Backend переведён на Go~~ Промежуточная редакция, заменена 2.3 |
| 2.0 | 08.10.2026 | Первоначальная редакция |
| 2.1 | 09.10.2026 | Хранилище поисковых документов и векторов: OpenSearch → Elasticsearch. `knn_vector` → `dense_vector` с `similarity: cosine`; HNSW остаётся, но это HNSW Elasticsearch. Ключ готовности в `GET /health/ready`: `checks.opensearch` → `checks.elasticsearch`. Сервис compose `opensearch` → `elasticsearch`. Остальные разделы не тронуты |

Причины правок: гибридный поиск BM25 + kNN с HNSW и слиянием RRF реализуется в Elasticsearch нативно, а переход на Go потребовал замены клиента OpenSearch. Ломающее изменение контракта — `checks.elasticsearch` — согласовано с участником №4. Редакция 2.3 снимает разделение на два языка: два рантайма в одном проекте означали бы дублирование контрактов, тестов и разбор двух стендов при каждой проверке.

**Участник №1 отвечает за конвертор документов, прием файлов, первичную настройку PostgreSQL и структурные миграции.**

## 1. Распределение ответственности

| № | Что реализует | Что получает | Что передает дальше |
|---|---|---|---|
| 1 | Каркас, PostgreSQL, миграции, файлы, конвертор, очередь заданий | Файл MD/DOCX/PDF и метаданные из API | `ParsedDocument`, задание `index`, доступ к репозиториям PostgreSQL |
| 2 | Чанки, embedding-провайдер, индекс Elasticsearch | `ParsedDocument` и `IndexingConfig` | Индексированные чанки, `IndexingResult`, интерфейсы `EmbeddingProvider` и `SearchIndex` |
| 3 | JWT, права, настройки, поиск, ответы LLM | Запрос пользователя, PostgreSQL, `EmbeddingProvider`, `SearchIndex` | `SearchResponse`, `AnswerResponse`, проверенный `Principal`, конфигурация обработки |
| 4 | Админка, виджет, демонстрационная страница | HTTP API и OpenAPI, пользовательский токен | Рабочие UI, JS-виджет и инструкция встраивания |

### 1.1. Архитектура

- Backend: **один Go-микросервис** (Go 1.27+) — HTTP, работа с документами, индексация, эмбеддинги, поиск, ответы LLM и worker. Python в проекте отсутствует.
- Хранилище метаданных: PostgreSQL. Схему применяет golang-migrate, SQL-миграции вшиты в бинарь через `//go:embed`.
- Хранилище поисковых документов и векторов: Elasticsearch.
- Frontend: React и TypeScript.
- API и worker — отдельные процессы одного пакета. Между модулями используем прямые вызовы, а не внутренние HTTP-сервисы. Между Go и внешними системами (Ollama, Elasticsearch, PostgreSQL) — только адаптеры.
- Внешние модели доступны через адаптеры: LLM — langchaingo поверх Ollama, эмбеддер — локальный ONNX-инференс sentence-transformers. Обучение собственных моделей не входит в MVP.
- Файлы оригиналов и результаты конвертации сохраняем на общем томе API и worker через `FileStorage`.
- Векторные данные в PostgreSQL не храним; pgvector для этой схемы не требуется.
- В проекте нет второго языка бэкенда: один рантайм, один набор контрактов, один стенд.

```text
backend/
  go/
    cmd/api/                     # HTTP-сервис
    cmd/worker/                  # воркер заданий
    internal/
      domain/                    # порты и типы, без импортов инфраструктуры
      application/               # сценарии: documents, search, answers, indexing, auth, settings
      infrastructure/            # postgres, elasticsearch, embeddings, llm, storage
      delivery/                  # httpapi, worker
      contracts/                 # типы, сгенерированные из OpenAPI
      migrations/                # SQL-миграции, вшиты в бинарь
    tests/
frontend/
  admin/                         # участник 4
  widget/                        # участник 4 — наш виджет
  demo/                          # участник 4 — сайт-хост, куда встраивается виджет
contracts/
  openapi.json
  examples/             # JSON-примеры входов и выходов
  fixtures/             # MD, DOCX, PDF и ожидаемые результаты
  test_keys/            # публичный тестовый ключ; без рабочих секретов

docs/
  settings.md
  deployment.md
  embedding-provider.md
  integration.md
compose.yaml
```

HTTP DTO экспортируем в OpenAPI, внутренние worker DTO — в JSON Schema и `contracts/examples`. Типы Go генерируются из `contracts/openapi.json`: спецификация — источник истины, обратная генерация не используется. Обязательность/nullability задаем в схемах, неизвестные поля запросов отклоняем. Участник 1 создает общий каркас и сборку API/worker. Участник 3 поддерживает общие DTO, Protocol и опубликованную OpenAPI; участники 1 и 2 реализуют объявленные интерфейсы. Участник 4 собирает frontend. Участник 1 ведет последовательность миграций; остальные передают ему изменения схемы вместе со своим кодом.

### 1.2. Ограничения MVP

- Принимаем MD UTF-8, DOCX и PDF с текстовым слоем.
- OCR сканов, прямые интеграции с Confluence/Drive/CRM, обучение моделей, диалоговая память, streaming и отдельная модель reranking в MVP не входят.
- Первый коннектор — прием файла по HTTP API. Архитектура позволяет добавить источник без изменения `ParsedDocument`.
- Одна база знаний использует один embedding-профиль. Смена модели или размерности после первой успешной индексации возвращает `409 EMBEDDING_PROFILE_CHANGE_REQUIRES_REBUILD`. Миграция всей базы между embedding-профилями — отдельная доработка.
- Инструмент первого релиза — `kb_search`. Нужные шаги оркестрации выполняем программно: разрешение настроек, проверка доступа, embedding вопроса, два вида поиска, объединение, LLM и проверка идентификаторов цитат.

## 2. Обязательные общие правила

### 2.1. Данные и безопасность доступа

- UUID используем для внутренних сущностей: tenant, KB, document, version, block, job. В JSON передаем строки.
- `user_id` и идентификаторы групп — строки внешней системы; они не обязаны быть UUID.
- JSON-поля — `snake_case`; даты — UTC ISO 8601; текст — UTF-8.
- `tenant_id` извлекаем из проверенного JWT. В запросах пользователь не выбирает чужой tenant.
- Роли `admin` и `user` действуют только внутри своего tenant. Администратор видит все базы и документы своего tenant.
- Для обычного пользователя база знаний доступна, если его `user_id` или хотя бы одна группа входит в ACL базы. Пустой ACL означает доступ только администраторам.
- ACL документа: `null` означает наследование ACL базы; непустой или пустой объект — собственный ACL документа. Документ не расширяет доступ за пределы ACL базы.
- Эффективный список доступных `document_id` участник 3 рассчитывает по текущим данным PostgreSQL. Участник 2 применяет этот список в Elasticsearch до поиска. Если список пуст, Elasticsearch не вызываем.
- Для MVP выбираем точную проверку ACL через список документов. Оптимизация больших ACL с материализацией групп в индексе — последующее расширение.
- Перед выдачей результатов и перед передачей текста LLM участник 3 повторно проверяет документ: tenant, текущие права, отсутствие удаления и активную версию. Если после проверки осталось меньше результатов, возвращаем меньше результатов.
- Публичные API не принимают готовый `Principal` из JSON и не возвращают эмбеддинги, пароли, внутренние пути тома и provider API keys.

### 2.2. Настройки

Настройки в БД заводит и изменяет системный аналитик. Разработчики создают структуру таблицы, реализуют чтение и документируют ключи, типы, подтипы, допустимые значения и поведение при отсутствии.

Запрещено автоматически создавать или изменять записи настроек при запуске, в миграциях или при запросе пользователя. Новые ключи сами по себе не требуют новой версии миграции. Админка показывает диагностику, но не редактирует настройки.

Рабочие секреты и адреса инфраструктуры передаем через окружение. `.env.example` содержит названия переменных и незаполненные места для секретов.

### 2.3. HTTP и ошибки

- API prefix: `/api/v1`.
- Все бизнес-методы требуют `Authorization: Bearer <JWT>`.
- Каждый ответ содержит `X-Request-ID`. Если входящий корректный UUID отсутствует, сервер генерирует новый.
- Списки используют `limit` от 1 до 100, по умолчанию 20, и непрозрачный `cursor`; сортировка `created_at DESC, id DESC`.
- Успешные ответы — JSON, кроме скачивания исходника.
- Удаленные, чужие и недоступные документы возвращают одинаковый `404 NOT_FOUND`.
- Неверный или просроченный токен — 401. Недостаточная административная роль — 403.
- Ошибки валидации тела запроса переводим в общий формат 422.

Общий формат ошибки:

```json
{
  "error": {
    "code": "CONFIGURATION_MISSING",
    "message": "Не заполнены обязательные настройки embedding-профиля",
    "details": {"keys": ["search.embedding_model"]}
  },
  "request_id": "88888888-8888-4888-8888-888888888888"
}
```

`details` содержит только разрешенные технические поля. Участник 3 реализует общий обработчик ошибок, остальные модули используют общие классы исключений.

### 2.4. Повтор запросов и заданий

- Для загрузки, замены содержимого, retry и reindex обязателен `Idempotency-Key`: строка длиной 1–128 символов.
- Ключ действует в рамках tenant, пользователя, HTTP-метода и пути. С тем же ключом и тем же содержимым возвращаем исходный ответ. С другим содержимым — `409 IDEMPOTENCY_CONFLICT`.
- Хеш запроса строим по байтам файла и каноническим метаданным; срок хранения ключа — 24 часа.
- Все worker-обработчики допускают повтор выполнения. Повтор не создает дополнительные версии и чанки.
- Вызовы провайдеров имеют таймаут; в MVP один запрос провайдера — максимум 60 секунд. Вызов из HTTP answer/search не повторяем автоматически.
- Worker повторяет временные ошибки максимум 3 попытки на этап; задержки 5 и 30 секунд. 429, timeout, network error и 5xx считаем временными; неправильную конфигурацию и несовместимую размерность — постоянными.

## 3. Контракты передачи данных

### 3.1. `Principal` — участник 3 → участники 1 и 3

Внутренний DTO; создается только после проверки JWT:

```json
{
  "tenant_id": "11111111-1111-4111-8111-111111111111",
  "user_id": "employee-42",
  "roles": ["user"],
  "groups": ["support"]
}
```

JWT: `sub` — user ID, `tenant_id` — UUID, `roles` и `groups` — массивы строк; обязательны `iss`, `aud`, `exp`. В MVP подпись RS256, ключ берем из конфигурации доверенного издателя, `aud = kb-search`. Время допуска на часы — 30 секунд. В неизвестных ролях не видим дополнительных полномочий.

### 3.2. `ParsedDocument` — участник 1 → участник 2

```json
{
  "schema_version": "1.0",
  "tenant_id": "11111111-1111-4111-8111-111111111111",
  "kb_id": "22222222-2222-4222-8222-222222222222",
  "document_id": "33333333-3333-4333-8333-333333333333",
  "version_id": "44444444-4444-4444-8444-444444444444",
  "title": "Инструкция по выгрузке",
  "filename": "export-guide.pdf",
  "content_sha256": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
  "source_url": "https://portal.example/guide/export",
  "metadata": {"product": "portal", "product_version": "4.2"},
  "blocks": [
    {
      "block_id": "55555555-5555-4555-8555-555555555555",
      "position": 0,
      "kind": "paragraph",
      "text": "Для настройки выгрузки откройте раздел Интеграции.",
      "section_path": ["Настройка", "Выгрузка"],
      "page": 3
    }
  ]
}
```

Правила:

- `source_url` — внешний информационный URL или `null`. Backend не загружает содержимое по этому URL.
- `page` — номер исходной страницы от 1 или `null`. Для MD и DOCX без достоверной пагинации передаем `null`.
- `kind` — `heading`, `paragraph`, `table`, `list`.
- `position` начинается с 0; блоки упорядочены и имеют уникальные значения.
- `section_path` — массив названий разделов, допускается пустой массив.
- `text` непустой после trim. Таблицы передаем как текст с заголовками и границами строк; сохранение изображения таблицы не требуется.
- Сохраняем внутренние переносы строк; переводим CRLF в LF. Не придумываем недоступные номера страниц или разделы.
- Одинаковый файл одной версии при повторной конвертации дает одинаковые блоки и `block_id`.
- При отсутствии извлеченного текста возвращаем `EMPTY_DOCUMENT` или `OCR_REQUIRED`, а не успешный пустой результат.

Для block_id используем UUIDv5 с namespace = version_id и именем из position, kind и SHA-256 текста. Участник 1 сохраняет JSON через `FileStorage` и указывает его ключ в задании. Worker участника 2 получает валидированный `ParsedDocument`, а не разбирает собственный формат конвертора.

### 3.3. `IndexingConfig` — участник 3 → участник 2

```json
{
  "embedding_profile_id": "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
  "embedding_provider_id": "primary",
  "embedding_model": "<идентификатор выбранной готовой модели>",
  "embedding_revision": "pilot-1",
  "embedding_dimension": 1024,
  "chunk_max_tokens": 600,
  "chunk_overlap_tokens": 100,
  "tokenizer_id": "<идентификатор выбранного готового токенизатора>",
  "normalize_vectors": true,
  "distance": "cosine"
}
```

1024 в примере — пример размерности, не обязательная размерность проекта. Аналитик указывает фактическую размерность выбранной модели.

Профиль вычисляем как SHA-256 JSON полей embedding_provider_id, embedding_model, embedding_revision, embedding_dimension, normalize_vectors, distance: UTF-8, ключи отсортированы, без пробелов между JSON-элементами. Токенизатор и правила чанкинга документируем отдельно; для одного задания сохраняем снимок всей конфигурации. Профиль нельзя подменить изменением настроек во время обработки.

### 3.4. `Chunk` — внутренний формат записи в Elasticsearch

```json
{
  "schema_version": "1.0",
  "chunk_id": "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc",
  "tenant_id": "11111111-1111-4111-8111-111111111111",
  "kb_id": "22222222-2222-4222-8222-222222222222",
  "document_id": "33333333-3333-4333-8333-333333333333",
  "version_id": "44444444-4444-4444-8444-444444444444",
  "chunk_number": 0,
  "title": "Инструкция по выгрузке",
  "text": "Для настройки выгрузки откройте раздел Интеграции.",
  "locator": {
    "block_ids": ["55555555-5555-4555-8555-555555555555"],
    "section_path": ["Настройка", "Выгрузка"],
    "page_start": 3,
    "page_end": 3
  },
  "embedding_profile_id": "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
  "embedding": [0.12, -0.34, 0.56]
}
```

Массив embedding здесь сокращен для чтения. В реальном объекте число координат равно `embedding_dimension`; значения конечные числа. `chunk_id` — строка sha256: плюс SHA-256 канонического JSON полей version_id, embedding_profile_id, tokenizer_id, chunk_max_tokens, chunk_overlap_tokens, chunk_number; те же правила канонизации, что у профиля. Его используем как Elasticsearch `_id`.

### 3.5. Интерфейсы адаптеров

DTO и `Protocol` объявляем в `backend/app/contracts/`. Все приведенные методы асинхронные. `Principal` передаем в бизнес-методы API; поисковый адаптер получает только уже рассчитанный фильтр.

| Интерфейс / метод | Вход | Выход | Реализует |
|---|---|---|---|
| `DocumentConverter.convert` | идентификаторы версии и ключ оригинала в `FileStorage` | `ParsedDocument` | 1 |
| `FileStorage.put/read/delete` | ключ и байты либо ключ | сохранение / байты / подтверждение удаления | 1 |
| `DocumentRepository.load_parsed` | tenant ID, version ID | `ParsedDocument` | 1 |
| `DocumentRepository.activate_version` | tenant ID, document ID, version ID, профиль, число чанков, lease token | подтверждение атомарного переключения | 1 |
| `DocumentRepository.get_allowed_active_versions` | `Principal`, KB ID | список `{document_id, active_version_id}` | 1, правила проверки предоставляет 3 |
| `EmbeddingProvider.embed` | `list[str]`, embedding-профиль | `list[list[float]]` в том же порядке | 2 |
| `SearchIndex.ensure_index` | embedding-профиль | имя индекса | 2 |
| `SearchIndex.upsert_chunks` | профиль, `list[Chunk]` | число подтвержденных записей | 2 |
| `SearchIndex.delete_version/delete_document` | tenant ID, document ID и при необходимости version ID | число удаленных записей | 2 |
| `SearchIndex.keyword_search` | текст, `SearchFilter`, limit | `list[SearchCandidate]` | 2 |
| `SearchIndex.vector_search` | вектор, профиль, `SearchFilter`, limit | `list[SearchCandidate]` | 2 |
| `Authorization.require_admin/require_read` | `Principal`, ресурс | подтверждение либо общий класс ошибки | 3 |
| `SettingsService.resolve_indexing` | tenant ID, KB ID | `IndexingConfig` | 3 |
| `SettingsService.resolve_search/resolve_answer` | tenant ID, KB ID | валидированная конфигурация операции | 3 |
| `LLMProvider.generate` | вопрос, доступные `SearchHit`, параметры модели | `{answer, citation_chunk_ids}` | 3 |

`SearchFilter` содержит обязательные tenant ID, KB ID, `embedding_profile_id`, `allowed_document_ids`. `SearchCandidate` содержит поля `Chunk` без embedding и с дополнительным `score`. Адаптер всегда возвращает кандидатов в порядке убывания своего score; score текстового и векторного поиска напрямую не сравниваем.

Общий job dispatcher реализует участник 1. В него регистрируем `convert_handler` участника 1, `index_handler` участника 2 и `delete_handler` участника 1. Компоненты собираем в `main.py`/`worker.py`, подключения к БД и моделям не создаем при импорте модулей.

### 3.6. Интерфейс фоновых заданий — участник 1 → участник 2

`JobRepository` должен предоставить следующие операции; интерфейсы и DTO объявляем в `internal/domain` и `internal/contracts`:

| Метод | Вход | Выход |
|---|---|---|
| `claim` | worker ID и список обрабатываемых stage | `JobLease` или null |
| `heartbeat` | job ID, lease token | bool: lease продлен или утрачен |
| `advance_to_index` | job ID, lease token, ключ ParsedDocument, IndexingConfig | тот же job в queued/index |
| `finish_index` | job ID, lease token, IndexingResult | job в succeeded; операция участвует в общей транзакции активации версии |
| `fail` | job ID, lease token, code, message, retryable | queued с next_attempt_at либо failed |
| `enqueue_cleanup` | tenant ID, document ID, version ID | cleanup job ID; повтор для той же версии возвращает прежний ID |

`JobLease`: job_id, tenant_id, document_id, version_id, kind, stage, attempt, lease_token, lease_expires_at. Для stage index dispatcher дополнительно загружает ParsedDocument и IndexingConfig через репозитории.

Index handler участника 2 получает `(job: JobLease, document: ParsedDocument, config: IndexingConfig)` и возвращает IndexingResult. Dispatcher завершает job через finish_index в той же транзакции, где DocumentRepository.activate_version переключает active_version_id. Ошибки handler передает общими исключениями, а dispatcher вызывает fail; handler не создает второй механизм retry.

Все операции изменения running job проверяют lease token. При утраченном lease возвращают JOB_LEASE_LOST; dispatcher прекращает обработку без изменения active version и без перевода уже выданного другому worker задания в failed.

## 4. Жизненный цикл документов и заданий

### 4.1. Версия документа

```text
uploaded → parsing → parsed → indexing → ready
```

Ошибка на этапе переводит версию в `failed` с кодом и сохраненным `failed_stage`. Повтор после ошибки запускает этап `convert`, если нет корректного ParsedDocument, иначе `index`. Версия `ready` доступна поиску только если совпадает с `documents.active_version_id`.

- `documents.latest_version_id` — последняя принятая версия, включая обрабатываемую или ошибочную.
- `documents.active_version_id` — последняя полностью индексированная версия или `null`.
- При обновлении активная версия продолжает работать до готовности новой.
- У документа допускаем одно незавершенное ingest-задание. Вторая загрузка/reindex/retry до завершения возвращает `409 DOCUMENT_BUSY`.
- Reindex готового документа создает новую версию из сохраненного оригинала с текущими параметрами чанкинга и тем же embedding-профилем.
- После переключения версии удаляем прежние чанки отдельным повторяемым cleanup-этапом. Сбой cleanup не отменяет готовность новой версии; поиск отсекает прежнюю версию по PostgreSQL.

### 4.2. Задание

`kind`: `ingest`, `delete`, `cleanup_version`.

`stage`: `convert`, `index`, `delete`, `cleanup`.

`status`: `queued`, `running`, `succeeded`, `failed`, `cancelled`.

При смене stage счетчик attempt начинается заново. При успешной конвертации тот же ingest `job_id` переходит в `queued` со stage `index`. После успешной индексации — `succeeded`. API не заставляет frontend искать ID второго задания.

Worker забирает задания транзакционно через блокировку строк; для running-задания фиксируем `lease_token`, `lease_expires_at`, heartbeat. Lease — 120 секунд, heartbeat — каждые 30 секунд. Истекший lease позволяет повторную выдачу; старый обработчик не может завершить задание или активировать версию с устаревшим token. Задание `cleanup_version` может иметь отдельный job ID и не меняет результат ingest.

При удалении документа сразу отмечаем `deleted_at` в PostgreSQL, отменяем ingest и создаем delete job. Уже после этого документ недоступен API и поиску. Delete handler удаляет чанки через участника 2; после завершения или истечения lease отмененных обработчиков удаляет оригиналы и ParsedDocument через `FileStorage`. Повторы delete безопасны.

## 5. ТЗ участнику №1: PostgreSQL, миграции и конвертор

### 5.1. Вход

1. Файл из HTTP multipart-запроса: MD/DOCX/PDF с текстовым слоем, максимум 20 MiB на файл в первом релизе.
2. `metadata` — JSON-строка с title, external ID, source URL и пользовательскими метаданными.
3. `Principal` и результаты проверки прав участника 3.
4. Подключение PostgreSQL, путь общего файлового тома.
5. Для worker — version ID и lease token задания конвертации. При принятии загрузки сохраняем снимок IndexingConfig; при конвертации и индексации используем именно его.

### 5.2. Выход

1. Из HTTP upload: `202`, document ID, version ID, job ID, `status = queued`.
2. В PostgreSQL: документ, версия, хеш оригинала, ingest job.
3. В `FileStorage`: оригинал и JSON `ParsedDocument`.
4. Для участника 2: stage `index`, ссылка на JSON и снимок `IndexingConfig` от участника 3.
5. Для всей команды: работающая схема PostgreSQL, миграции, репозитории, FileStorage, dispatcher и Docker Compose.

### 5.3. Первичная схема PostgreSQL

Участник 1 создает таблицы и ограничения. Приведенные поля — обязательный минимум; технические timestamps могут добавляться.

| Таблица | Поля и ограничения |
|---|---|
| `tenants` | `id uuid PK`, `name text` |
| `knowledge_bases` | `id uuid PK`, `tenant_id uuid FK`, `name text`, `allowed_user_ids jsonb`, `allowed_group_ids jsonb`, `embedding_profile_id text NULL`, `created_at timestamptz` |
| `documents` | `id uuid PK`, `tenant_id`, `kb_id`, `external_id text NULL`, `acl_override jsonb NULL`, `active_version_id uuid NULL`, `latest_version_id uuid NULL`, `deleted_at timestamptz NULL`, `created_at` |
| `document_versions` | `id uuid PK`, `tenant_id`, `document_id`, `filename`, `title`, `source_url NULL`, `metadata jsonb`, `content_sha256`, `original_key`, `parsed_key NULL`, `status`, `failed_stage NULL`, `error_code NULL`, `error_message NULL`, `embedding_profile_id NULL`, `indexing_config jsonb NULL`, `chunk_count integer`, `created_at` |
| `jobs` | `id uuid PK`, `tenant_id`, `document_id`, `version_id NULL`, `kind`, `stage`, `status`, `attempt`, `next_attempt_at`, `lease_token NULL`, `lease_expires_at NULL`, `processed_chunks`, `total_chunks`, `error_code NULL`, `error_message NULL`, timestamps |
| `settings` | `id uuid PK`, `tenant_id`, `kb_id NULL`, `key text`, `value_type text`, `subtype text`, `value jsonb`, `updated_at` |
| `api_idempotency` | tenant, user, method, path, key, request hash, HTTP status, JSON response, expiry; составной уникальный ключ |

Обязательные ограничения:

- FK и проверки исключают связь документа с KB другого tenant и версии с чужим документом.
- Уникальный непустой `external_id` внутри KB, включая soft-deleted документы. Повторная загрузка с таким external ID возвращает `409 DOCUMENT_EXISTS`; для обновления используется PUT по document ID.
- Не более одной записи настройки с ключом на уровне tenant; не более одной с тем же ключом на уровне конкретной KB. Уровень `kb_id = NULL` учитываем отдельным уникальным индексом.
- Только одна активная ingest-операция на документ; конкурентные запросы проверяем под блокировкой строки.
- Таблица `settings` после миграции пустая. Рабочие tenants, ACL и настройки не засеваем миграциями; тестовые fixtures допускаются только внутри тестовой БД.

### 5.4. API документов

Доступ на изменение — admin. На чтение — admin или текущий ACL.

| Метод | Вход | Успешный выход |
|---|---|---|
| `POST /kbs/{kb_id}/documents` | multipart `file`, `metadata`; Idempotency-Key | 202 `DocumentAccepted` |
| `GET /documents?kb_id=...&limit=...&cursor=...` | KB ID и пагинация | 200 `{items: DocumentInfo[], next_cursor}`; только доступные документы |
| `GET /documents/{id}` | document ID | 200 `DocumentInfo` |
| `PUT /documents/{id}/content` | новый multipart file и metadata; Idempotency-Key | 202 `DocumentAccepted` с новой версией |
| `POST /documents/{id}/retry` | пустое JSON `{}`; Idempotency-Key | 202 `DocumentAccepted`; возобновляется failed latest version |
| `POST /documents/{id}/reindex` | пустое JSON `{}`; Idempotency-Key | 202 `DocumentAccepted` с новой версией из оригинала |
| `DELETE /documents/{id}` | document ID | 202 `{document_id, job_id, status: "deleting"}` |
| `GET /jobs/{id}` | job ID | 200 `JobInfo`; только admin своего tenant |
| `GET /documents/{id}/source?version_id=...` | document ID, необязательный version ID | 200 файл как attachment |

Для source обычному пользователю доступна только активная версия. Admin может получить любую версию своего неудаленного документа. Чужой, удаленный или недоступный документ всегда 404. Запрос без version ID выбирает активную версию; если ее нет — latest version только для admin.

Вход metadata загрузки:

```json
{
  "title": "Инструкция по выгрузке",
  "external_id": "guide-export-42",
  "source_url": "https://portal.example/guide/export",
  "metadata": {"product": "portal", "product_version": "4.2"}
}
```

Title обязателен, 1–300 символов. External ID — непустая строка до 300 символов или null. Source URL — абсолютный URL с протоколом https/http либо null; другие протоколы запрещены. External ID и source URL могут отсутствовать. Пользовательские metadata — JSON object размером не более 16 KiB. PUT не меняет external ID документа. Внешние ACL из произвольных metadata не применяем.

`DocumentAccepted`:

```json
{
  "document_id": "33333333-3333-4333-8333-333333333333",
  "version_id": "44444444-4444-4444-8444-444444444444",
  "job_id": "66666666-6666-4666-8666-666666666666",
  "status": "queued",
  "request_id": "88888888-8888-4888-8888-888888888888"
}
```

`DocumentInfo`: `document_id`, `kb_id`, `external_id`, `active_version_id`, `latest_version_id`, `created_at`, `latest_version`, `active_version`. Объекты версий содержат `version_id`, `title`, `filename`, `status`, `chunk_count`, `error_code`, `error_message`; active version может быть `null`. В публичном ответе нет ключей файлового тома.

`JobInfo`: `job_id`, `document_id`, `version_id`, `kind`, `stage`, `status`, `attempt`, `processed_chunks`, `total_chunks`, `error_code`, `error_message`, `updated_at`. При неизвестном количестве чанков `total_chunks = null`; не показываем выдуманный процент.

### 5.5. Обработка и ошибки

1. Проверить роль, KB и конфигурацию индексации до принятия ingest. При отсутствующих настройках ответить 409.
2. Проверить размер, формат и возможность открыть файл. Не использовать клиентский filename как путь на диске.
3. Сохранить оригинал под серверным ключом, создать записи версии и задания транзакционно. Если транзакция не состоялась, удалить сохраненный файл; временные файлы чистить при повторном запуске.
4. Конвертировать готовой библиотекой, привести результат к `ParsedDocument`.
5. Сохранить JSON и перевести то же задание в stage index. Конвертация не вызывает embedding-модель.
6. Регистрировать ошибки: `UNSUPPORTED_FORMAT` (415), `FILE_TOO_LARGE` (413), `INVALID_METADATA` (422). Обнаруженные после загрузки `CORRUPT_DOCUMENT`, `EMPTY_DOCUMENT`, `OCR_REQUIRED` отражать в версии и JobInfo со status failed.

### 5.6. Критерии сдачи

- Команды разворачивания из раздела 10 выполняются на пустой БД.
- Один MD, DOCX и текстовый PDF дают валидный ParsedDocument с сохраненным порядком текста.
- Для PDF сохраняются достоверные номера страниц; для остальных нет выдуманной пагинации.
- Повтор POST с одним Idempotency-Key возвращает исходные IDs; измененный файл с тем же ключом дает 409.
- Worker после падения допускает повтор конвертации без дублей и без завершения устаревшим lease token.
- Для failed conversion GET jobs возвращает конкретный код ошибки.
- Тесты выполняются с mock Authorization/SettingsService и без моделей/Elasticsearch.
- Передать участнику 2 минимум три файла fixtures, соответствующие JSON ParsedDocument и тестовый ingest job на этапе index.

## 6. ТЗ участнику №2: чанки, эмбеддинги и индекс

### 6.1. Вход

- Валидный ParsedDocument, предоставленный участником 1.
- Зафиксированный IndexingConfig, предоставленный участником 3.
- Stage index и действующий lease token из общей очереди.
- Endpoint Elasticsearch и данные подключения embedding-провайдера из окружения.
- Для поисковых методов: текст или embedding вопроса, SearchFilter и limit от участника 3.

### 6.2. Выход

- Чанки в Elasticsearch по контракту Chunk.
- `IndexingResult`: `document_id`, `version_id`, `embedding_profile_id`, `chunk_count`, `status = ready`.
- Активированная версия в PostgreSQL через DocumentRepository.
- Stage index завершен; job status succeeded.
- Реализованные EmbeddingProvider и SearchIndex для участника 3.

### 6.3. Алгоритм индексации

1. Проверить, что версия и документ не удалены, а lease принадлежит обработчику.
2. Пройти blocks по position. Не объединять разные section_path в один чанк. Один длинный блок разделять готовым токенизатором. В пределах раздела объединять соседние блоки до chunk_max_tokens; overlap применять только внутри того же раздела. Для каждого фрагмента сохранить block IDs и диапазон достоверных страниц.
3. Запретить пустые чанки. `0 <= overlap < max_tokens`; неверная настройка — CONFIGURATION_INVALID.
4. Вычислить детерминированные chunk IDs.
5. Вызвать EmbeddingProvider пакетами по 32 текста. Проверить количество ответов, порядок, размерность, конечность координат и ненулевую норму. При включенной normalize привести векторы к единичной L2-норме.
6. Создать/проверить индекс профиля и записать чанки через bulk upsert. Проверить результат каждого bulk item; частичный успех не считать завершением.
7. Убедиться, что все чанки доступны поисковому запросу после refresh, и вернуть IndexingResult общему dispatcher. Dispatcher участника 1 выполняет активацию и finish_index в одной транзакции: блокирует строки KB, document и job, проверяет version_id = latest_version_id, действующий lease token и отсутствие удаления; фиксирует профиль KB при первой активации либо требует совпадение уже зафиксированного профиля. Это исключает публикацию двух разных профилей при параллельной первой загрузке. Сам index handler не переключает active_version_id и не завершает job отдельно.
8. В той же транзакции dispatcher ставит cleanup прежней версии через JobRepository участника 1. Не ставить cleanup, если прежняя версия отсутствует или совпадает с активируемой. Cleanup handler перед удалением дополнительно убеждается, что удаляемая версия не активна.
9. Если ошибка прервала этап, прежняя active version сохраняется. Частичные чанки новой версии не проходят проверку active_version_id у участника 3.

### 6.4. Elasticsearch

- Имя индекса: `kb-chunks-v1-<hex sha256 профиля без префикса sha256:>`.
- Обязательные keyword-поля: tenant ID, KB ID, document ID, version ID, chunk ID, embedding profile ID.
- `text` и `title` — полнотекстовые поля с русским анализатором; необходимые точные идентификаторы храним отдельными keyword-полями.
- `embedding` — `dense_vector` фиксированной dimension с `similarity: cosine`; индекс HNSW Elasticsearch. Участник 2 фиксирует совместимую версию Elasticsearch и настройки mapping в репозитории. При dimension >= 384 Elasticsearch выбирает `bbq_hnsw`, иначе — `int8_hnsw`.
- keyword_search и vector_search обязаны фильтровать tenant, KB, профиль и allowed_document_ids. Пустой список разрешенных ID означает пустой результат.
- keyword_search возвращает кандидатов по тексту; vector_search — по переданному вектору. Слияние результатов выполняет участник 3.
- Source URL и ссылка для скачивания не извлекаются из произвольных индексных полей для публичного ответа: участник 3 собирает их по актуальным метаданным PostgreSQL.

### 6.5. Ошибки

- `EMBEDDING_DIMENSION_MISMATCH`, `EMBEDDING_RESPONSE_INVALID`, `INDEX_PROFILE_MISMATCH` — постоянные ошибки этапа.
- `EMBEDDING_PROVIDER_UNAVAILABLE`, `INDEX_UNAVAILABLE` — повторяемые ошибки worker согласно разделу 2.4.
- Неизвестная version или удаленный документ — cancelled, без активации.
- При смене профиля в настройках существующей KB ответить общим конфликтом профиля; не создавать впечатление успешной совместимости старых и новых векторов.

### 6.6. Критерии сдачи

- Один ParsedDocument индексируется, chunk_count совпадает с количеством записанных чанков.
- Повтор одной index-операции дает те же IDs и не увеличивает количество записей.
- Длинный блок разбивается без превышения лимита; локаторы позволяют найти исходное место.
- Несовместимая размерность и частичная ошибка bulk не активируют версию.
- Фильтрация исключает документ другого tenant и документ вне allowed IDs.
- После переключения версии поисковый модуль может отличить старые и новые кандидаты.
- delete_document удаляет все версии и допускает повтор.
- Передать участнику 3 mock адаптеры, два списка SearchCandidate, индекс fixtures и инструкцию подключения выбранной готовой embedding-модели.

## 7. ТЗ участнику №3: настройки, права, поиск и ответы

### 7.1. Вход

- JWT от внешней системы и JSON запрос поиска/ответа.
- PostgreSQL-репозитории участника 1 и записи настроек, внесенные аналитиком.
- EmbeddingProvider и SearchIndex участника 2.
- Endpoint и секрет готовой LLM из окружения.

### 7.2. Выход

- Principal для backend и проверки доступа для API участника 1.
- IndexingConfig для участника 2.
- SearchResponse для пользователя и участника 4.
- AnswerResponse с цитатами для пользователя и участника 4.
- OpenAPI, DTO, mock ответы, каталог настроек и общие обработчики ошибок.

### 7.3. API и входные параметры

| Метод | Вход | Выход |
|---|---|---|
| `GET /me` | JWT | Principal |
| `POST /kbs` | `{name, allowed_user_ids, allowed_group_ids}`; admin | 201 `{kb_id, name, allowed_user_ids, allowed_group_ids}` |
| `GET /kbs` | JWT, limit, cursor | `{items: KBInfo[], next_cursor}`; только доступные KB |
| `PATCH /kbs/{kb_id}/acl` | `{allowed_user_ids, allowed_group_ids}`; admin | 200 обновленный ACL |
| `PATCH /documents/{id}/acl` | `{acl_override: null}` или объект с allowed_user_ids/group_ids; admin | 200 `{document_id, acl_override}` |
| `POST /search` | SearchRequest | SearchResponse |
| `POST /answers` | AnswerRequest | AnswerResponse |
| `GET /settings/diagnostics?kb_id=...` | admin, KB ID | `{valid, missing_keys, invalid_keys, embedding_profile_compatible}` |

KBInfo: `kb_id`, `name`, `created_at`. ACL меняем через бизнес-методы; настройки через эти методы не меняем. Создание KB не создает автоматически записи настроек.

SearchRequest:

```json
{
  "kb_id": "22222222-2222-4222-8222-222222222222",
  "query": "Как настроить выгрузку?",
  "top_k": 10
}
```

`query`: 1–2000 символов после trim. `top_k`: необязательный integer 1–50; если отсутствует, используем search.top_k из БД. Дополнительные фильтры metadata в первом API не реализуем. AnswerRequest содержит `kb_id`, `question` с тем же ограничением длины; top_k берем из настроек.

### 7.4. Поиск

1. Проверить JWT и доступ к KB.
2. Получить active embedding profile и валидные настройки; вычислить текущие доступные document IDs и версии по PostgreSQL.
3. Если доступных активных документов нет, вернуть status no_results с пустым hits.
4. Получить embedding вопроса через тот же EmbeddingProvider и профиль, что при индексации.
5. Параллельно вызвать keyword_search и vector_search с limit `min(100, 5 * top_k)` и одинаковым SearchFilter.
6. Объединить по chunk_id через RRF: `score = 1/(60 + lexical_rank) + 1/(60 + vector_rank)`, ранги от 1. Отсутствующую ветку для конкретного чанка считать нулем. При равном score сортировать по chunk_id.
7. Отбросить неактивные версии и повторно проверить актуальный доступ. Вернуть первые top_k, без повторного дозапроса в MVP.
8. Source URL брать из version metadata, а download URL формировать как защищенный API source с version ID.

Если embedding или Elasticsearch недоступен, ответить 503 с общим кодом; не выдавать молча частичный поиск как полный.

SearchResponse:

```json
{
  "status": "ok",
  "hits": [
    {
      "chunk_id": "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc",
      "document_id": "33333333-3333-4333-8333-333333333333",
      "version_id": "44444444-4444-4444-8444-444444444444",
      "title": "Инструкция по выгрузке",
      "text": "Для настройки выгрузки откройте раздел Интеграции.",
      "locator": {"block_ids": ["55555555-5555-4555-8555-555555555555"], "section_path": ["Настройка", "Выгрузка"], "page_start": 3, "page_end": 3},
      "source_url": "https://portal.example/guide/export",
      "download_url": "/api/v1/documents/33333333-3333-4333-8333-333333333333/source?version_id=44444444-4444-4444-8444-444444444444",
      "score": 0.032786885
    }
  ],
  "request_id": "88888888-8888-4888-8888-888888888888"
}
```

При пустой выдаче `status = no_results`, `hits = []`. Score — значение RRF, не вероятность правильности.

### 7.5. Ответ LLM

1. Проверить, что tools.enabled содержит kb_search, и выполнить поиск тем же SearchService.
2. При пустом hits вернуть 200, status no_evidence, answer «В доступной базе знаний не найдено подтверждающих материалов.», citations пустой массив. LLM не вызывать.
3. Передать готовой LLM вопрос и доступные чанки с их IDs; ограничить общий контекст answer.max_context_tokens готовым токенизатором модели. Добавлять чанки в порядке поиска целиком; если ни один не помещается, вернуть 409 CONFIGURATION_INVALID.
4. Попросить структурированный ответ `{answer, citation_chunk_ids}`. Тексты документов трактовать как данные; они не меняют разрешенные tools и права.
5. Проверить JSON, непустой answer и citation IDs. Каждый ID должен присутствовать в переданных модели чанках. Ответ без цитат или с чужим ID вернуть как no_evidence с нейтральным сообщением, без неподтвержденного текста модели.
6. Повторно проверить права и активность цитируемых версий перед возвратом. При изменении вернуть no_evidence; не пересобирать ответ из недоступного текста.
7. Собрать citations на сервере. Проверка IDs подтверждает существование источника, но не является гарантией истинности каждого утверждения; качество проверяем на приемочных вопросах.

AnswerResponse:

```json
{
  "status": "ok",
  "answer": "Откройте раздел Интеграции и настройте параметры выгрузки [1].",
  "citations": [
    {
      "number": 1,
      "chunk_id": "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc",
      "document_id": "33333333-3333-4333-8333-333333333333",
      "version_id": "44444444-4444-4444-8444-444444444444",
      "title": "Инструкция по выгрузке",
      "quote": "Для настройки выгрузки откройте раздел Интеграции.",
      "locator": {"block_ids": ["55555555-5555-4555-8555-555555555555"], "section_path": ["Настройка", "Выгрузка"], "page_start": 3, "page_end": 3},
      "source_url": "https://portal.example/guide/export",
      "download_url": "/api/v1/documents/33333333-3333-4333-8333-333333333333/source?version_id=44444444-4444-4444-8444-444444444444"
    }
  ],
  "request_id": "88888888-8888-4888-8888-888888888888"
}
```

Нумерацию `[1]`, `[2]` валидируем относительно citation IDs либо формируем сервером по согласованному структурированному ответу. Quote — реальный текст исходного чанка, сформированный сервером, не придуманная цитата LLM. Недоступная LLM — 503 LLM_UNAVAILABLE. Невалидный JSON ответа — 502 LLM_RESPONSE_INVALID. Search API продолжает работать независимо от генерации.

### 7.6. Критерии сдачи

- Неверная подпись, issuer, audience и просроченный JWT возвращают 401.
- Обычный пользователь не выполняет admin-методы.
- API исключает чужой tenant; пустой ACL не открывает базу всем.
- Изменение ACL сразу влияет на следующий поиск без переиндексации.
- В поиске используются активные версии и одинаковый embedding-профиль.
- RRF дает воспроизводимый порядок на заданных mock списках.
- На вопрос без доступных фрагментов LLM не вызывается.
- Чужой citation ID не проходит проверку; quote собирается из реального чанка.
- При отключении LLM обычный search API работает.
- Передать участнику 4 OpenAPI и JSON fixtures для success, no_results, no_evidence, 401, 403, 409, 422, 502 и 503.

## 8. ТЗ участнику №4: админка и виджет

### 8.1. Вход

- OpenAPI и JSON fixtures участников 1 и 3.
- Базовый URL API.
- `get_token(): Promise<string>` от приложения клиента; возвращает действующий пользовательский JWT.
- Для виджета: DOM-контейнер, KB ID, тема и необязательный callback открытия источника.

### 8.2. Выход

- Собранная админка, доступная отдельным web-приложением.
- Файл `kb-search-widget.js` с самостоятельной сборкой и изолированными стилями.
- Демонстрационная HTML-страница вне админки.
- Инструкция подключения и список используемых API.
- Моки HTTP API для разработки без готового backend.

### 8.3. Админка: действие → вход API → результат UI

| Действие | API | Что показываем |
|---|---|---|
| Вход | get_token, GET /me | роль и доступные экраны; admin-раздел только для admin |
| Выбор базы | GET /kbs | список доступных баз |
| Создание базы | POST /kbs | новая база; без автоматического заполнения настроек |
| Права базы/документа | PATCH соответствующего ACL | список пользователей/групп и подтверждение сохранения |
| Загрузка | POST /kbs/{id}/documents | ID документа и job; status queued |
| Список документов | GET /documents | latest и active версии, статус, число чанков, ошибка |
| Обновление | PUT /documents/{id}/content | новая latest version и текущая active version |
| Повтор failed | POST /documents/{id}/retry | job и этап возобновления |
| Переиндексация | POST /documents/{id}/reindex | новая версия и job |
| Удаление | DELETE /documents/{id} | документ скрыт после подтверждения 202 |
| Диагностика настроек | GET /settings/diagnostics | список отсутствующих/невалидных ключей; без редактирования |

Для операций с Idempotency-Key создавать UUID на новое действие. При сетевом повторе того же действия сохранять ключ и исходные данные; при новом действии создавать новый ключ.

GET jobs опрашивать каждые 2 секунды только для незавершенных заданий и пока нужный экран открыт. Останавливать polling при succeeded, failed, cancelled и unmount. Не показывать процент, если total_chunks неизвестен.

### 8.4. Поиск и ответы

- Поле запроса и явное переключение «Поиск» / «Вопрос».
- Поиск вызывает POST /search; показывает hits с title, text, section/page и ссылками.
- Вопрос вызывает POST /answers; показывает answer и раскрываемые citations.
- no_results и no_evidence — отдельные пустые состояния без технических traceback.
- 401: один раз обновить токен через get_token и повторить безопасный запрос; после второй 401 показать необходимость входа. Мутацию повторять только с прежним Idempotency-Key.
- 403: показать отсутствие полномочий. 409: показать конкретный конфликт или недостающие настройки. 502/503: показать ошибку сервиса и request ID.
- При LLM_UNAVAILABLE предложить отдельной кнопкой обычный поиск по тому же вопросу; не скрывать сбой генерации.
- Source/download открывать с авторизацией. Для защищенного скачивания использовать fetch с Bearer и Blob, а не ссылку без токена.
- Ответы и фрагменты выводить как текст или безопасно очищенный Markdown. Произвольный HTML из документов не исполнять.

### 8.5. Контракт виджета

```typescript
interface WidgetOptions {
  container: HTMLElement;
  api_url: string;
  kb_id: string;
  get_token: () => Promise<string>;
  theme?: 'light' | 'dark';
  on_open_source?: (citation: Citation) => void;
}

interface WidgetHandle {
  destroy(): void;
}

window.KBSearchWidget.mount(options: WidgetOptions): WidgetHandle;
```

mount создает UI только внутри container. Стили изолируем через Shadow DOM. destroy снимает обработчики и отменяет запросы/polling. По умолчанию theme light. Токен не сохраняем в localStorage и не передаем в URL.

Пример интеграции:

```html
<div id="kb-search"></div>
<script src="https://search.example/assets/kb-search-widget.js"></script>
<script>
  const widget = window.KBSearchWidget.mount({
    container: document.getElementById('kb-search'),
    api_url: 'https://search.example/api/v1',
    kb_id: '22222222-2222-4222-8222-222222222222',
    get_token: async () => {
      const response = await fetch('/internal/kb-token');
      if (!response.ok) throw new Error('Не удалось получить токен');
      const result = await response.json();
      return result.token;
    }
  });
</script>
```

`/internal/kb-token` реализует система клиента: она подтверждает своего пользователя и выдает подписанный токен. Это не endpoint нашего поиска. Для demo участник 3 предоставляет тестовый выпуск токенов; рабочие секреты в JS не встраиваем. Участник 1 включает адрес demo в разрешенный CORS origin, без wildcard для рабочего окружения.

### 8.6. Критерии сдачи

- Админка проходит загрузку → polling → ready → поиск → обновление → удаление.
- Моки и реальный backend используют одинаковые DTO.
- Виджет на отдельной HTML-странице выполняет search и answers.
- CSS страницы и виджета не меняет оформление друг друга.
- Истекший токен обрабатывается без бесконечных повторов.
- Недоступный источник не открывается без токена.
- Два экземпляра виджета работают независимо; destroy прекращает их запросы.
- Проверены состояния no_results, no_evidence, failed job и LLM_UNAVAILABLE.

## 9. Каталог настроек для аналитика

Записи настроек не создаем программно. Значения таблицы — условия валидации и рекомендации для пилота, не seed-данные. Участник 3 оформляет этот каталог в `docs/settings.md`.

Приоритет: настройка конкретной KB → настройка tenant. Глобальных настроек других tenants не читаем. Если обязательного ключа нет на обоих уровнях, операция возвращает 409 CONFIGURATION_MISSING с перечнем отсутствующих ключей. Невалидное значение — 409 CONFIGURATION_INVALID. Молчаливых значений по умолчанию для этих ключей нет.

| Ключ | Тип | Подтип | Значение / ограничение | Нужен для |
|---|---|---|---|---|
| `search.embedding_provider_id` | string | model | ID адаптера, например primary; должен существовать в окружении | index, search |
| `search.embedding_model` | string | model | точный ID готовой модели у провайдера | index, search |
| `search.embedding_revision` | string | model | зафиксированная версия или согласованная метка конфигурации; менять только с rebuild | index, search |
| `search.embedding_dimension` | integer | model | фактическая dimension модели, положительное число в пределах выбранного mapping | index, search |
| `search.embedding_normalize` | boolean | model | для пилота true | index, search |
| `search.embedding_distance` | string | model | в MVP только cosine | index, search |
| `chunking.tokenizer_id` | string | ingestion | ID готового токенизатора, поддерживаемого приложением и соответствующего модели | index |
| `chunking.max_tokens` | integer | ingestion | для пилота 600; положительное, не больше лимита embedding-модели | index |
| `chunking.overlap_tokens` | integer | ingestion | для пилота 100; от 0 до max_tokens − 1 | index |
| `search.top_k` | integer | retrieval | для пилота 10; от 1 до 50 | search, answer |
| `answer.provider_id` | string | model | ID зарегистрированного LLM-адаптера | answer |
| `answer.model` | string | model | точный ID готовой LLM | answer |
| `answer.tokenizer_id` | string | generation | готовый токенизатор/счетчик для выбранной LLM | answer |
| `answer.max_context_tokens` | integer | generation | для пилота 6000; должно помещаться вместе с вопросом, инструкциями и выходом в окно модели | answer |
| `answer.max_output_tokens` | integer | generation | для пилота 1000; положительное, в пределах провайдера | answer |
| `tools.enabled` | array[string] | tools | в MVP допускается kb_search; для ответов должно быть ["kb_search"] | answer |

Основные переменные окружения: DATABASE_URL, OPENSEARCH_URL, OPENSEARCH_USERNAME, OPENSEARCH_PASSWORD, FILE_STORAGE_ROOT, EMBEDDING_PRIMARY_BASE_URL, EMBEDDING_PRIMARY_API_KEY, LLM_PRIMARY_BASE_URL, LLM_PRIMARY_API_KEY, JWT_ISSUER, JWT_AUDIENCE, JWT_PUBLIC_KEY_FILE, CORS_ALLOWED_ORIGINS. Рабочие API keys и пароли не коммитим.

До обращения к моделям участники 2 и 3 должны выбрать один реальный embedding-провайдер и один LLM-провайдер, указать их в `docs/embedding-provider.md`, зафиксировать ID моделей, их лимиты, токенизаторы и фактическую размерность. Это единственное внешнее условие живого пилота; mocks позволяют выполнять контрактную приемку до получения доступа. Участник 2 реализует embedding-адаптер выбранного провайдера, участник 3 — LLM-адаптер.

## 10. Сборка и порядок интеграции

### 10.1. Команды, которые должна поддерживать сборка

Участник 1 предоставляет `.env.example`, Dockerfiles и compose-сервисы postgres, elasticsearch, migrate, api, worker. Участник 4 добавляет admin и demo. В образах фиксируем версии; оператор перед запуском заполняет окружение.

```bash
docker compose up -d postgres elasticsearch
docker compose run --rm migrate
docker compose up -d api worker admin demo
```

`migrate` выполняет `alembic upgrade head`, создает структуру и не создает настройки. После этого аналитик вручную вносит рабочие настройки; приложение не требует их наличия для самого запуска. Создание KB возможно до настройки моделей; ingest/search/answers возвращают понятную ошибку до настройки соответствующей операции.

GET /health/live не требует токена и подтверждает работу процесса. GET /health/ready проверяет PostgreSQL и Elasticsearch (ключ ответа `checks.elasticsearch`); 200 при доступности, 503 иначе. Отсутствие настроек и ключа модели показывает авторизованная diagnostics, а не публичный health. При недоступной LLM HTTP API поиска остается доступен.

### 10.2. Порядок работы без блокировки участников

1. Участник 3 публикует DTO, Protocol, OpenAPI и mock ответы. Участник 1 создает каркас, схему и тестовую очередь.
2. Участник 1 делает конвертор на файлах fixtures; участник 2 делает индексатор на готовом mock ParsedDocument; участник 3 делает поиск на mock SearchIndex; участник 4 делает UI на mock API.
3. Участники 1 и 2 соединяют конвертор с индексатором через общий worker.
4. Участники 2 и 3 соединяют индекс с поиском и embedding вопроса.
5. Участник 4 заменяет mock API на реальный backend без изменения UI DTO.
6. Четверо выполняют общую приемку.

### 10.3. Общая приемка

Для живой приемки аналитик внес настройки, выбраны готовые модели и предоставлены ключи. Участник 3 предоставляет локальный тестовый issuer/fixtures JWT, без рабочих приватных ключей в репозитории.

1. Создать tenant в тестовой БД, admin и два пользовательских JWT: support и sales. Создать KB с доступом support через POST /kbs.
2. Admin загружает тестовую инструкцию. Получаем DocumentAccepted; JobInfo проходит convert → index → succeeded.
3. Support ищет известную фразу и получает нужный документ и страницу/раздел.
4. Support задает вопрос и получает ответ с существующим citation ID и открывающимся исходником.
5. Sales не получает этот документ ни поиском, ни скачиванием; пользователь другого tenant также не получает его.
6. Admin обновляет документ. Во время обработки доступна прежняя версия; после готовности search возвращает только новую активную версию.
7. На неправильном embedding-векторе и ошибке bulk новая версия не активируется.
8. Admin отзывает доступ support. Следующий search не возвращает документ без переиндексации.
9. Отключить LLM: answers возвращает LLM_UNAVAILABLE, search продолжает работать.
10. Удалить документ: сразу нет доступа в API/search; delete job очищает индекс и файлы.
11. Повторить upload с тем же Idempotency-Key и index job: нет дублей.
12. Прервать worker и дождаться истечения lease: обработку можно завершить новым worker; старый token не активирует версию.

Сдать код своего модуля, значимые тесты приемки, инструкции запуска и JSON-примеры входов/выходов. Часть считается готовой, когда следующий участник использует ее через опубликованный контракт без чтения внутренних деталей реализации.
