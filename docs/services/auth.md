# auth

Выпуск и проверка сессий. Владелец таблицы `auth_users` и единственное место, где
роль превращается в набор аудиторий (`scp`).

## Ответственность

- `POST /api/v1/auth/login` — обмен логин/пароля на JWT (RS256) в httpOnly-cookie.
- `POST /api/v1/auth/logout` — сброс cookie.
- `GET /api/v1/auth/me` — текущий пользователь по cookie.
- `GET /.well-known/jwks.json` — публичные ключи для проверки подписи.
- `GET /healthz`, `/actuator/*`, `/metrics`.

Не хранит сессии (stateless JWT), не знает про документы и поиск.

## Контракт

Токен — RS256-JWT. Claims: `sub`, `name`, `role`, `scp`, `tenant`, `kb`, `exp`.
`scp` — аудитории для фильтрации: `employee` и `admin` получают
`[public, employee]`, остальные — `[public]`. Потребители (retrieval, answer,
documents) фильтруют по `scp`, а не по роли — второго источника истины нет.

Ответы ошибок — `{"error": {"code", "message"}, "request_id"}`.

## Роль и аудитория

| Роль       | scp                  | Пояснение         |
| ---------- | -------------------- | ----------------- |
| `user`     | `public`             | Клиент            |
| `employee` | `public`, `employee` | Сотрудник         |
| `admin`    | `public`, `employee` | Сервисный аккаунт |

## Админ-аккаунт (dev)

В dev-профиле сидится пользователь `admin` с ролью `admin`. Его токен живёт
`auth.admin-session-ttl` (по умолчанию `3153600000` секунд ≈ 100 лет) — это
намеренное исключение для сервисных вызовов на стенде. Пароль берётся из
`AUTH_DEV_ADMIN_PASSWORD` (по умолчанию `dev-admin-password`).

Сиды (`user`, `employee`, `admin`) выполняются **только** с профилем `dev`
(`@Profile("dev")`). Без него аккаунты заводятся вручную.

## Конфигурация

| Переменная                     | По умолчанию         | Назначение                             |
| ------------------------------ | -------------------- | -------------------------------------- |
| `SPRING_PROFILES_ACTIVE`       | —                    | `dev` включает сиды демо-пользователей |
| `AUTH_TENANT_ID`               | `1111…1111`          | Tenant по умолчанию                    |
| `AUTH_KB_ID`                   | `74ade…5868`         | База знаний по умолчанию               |
| `AUTH_SESSION_TTL`             | `28800`              | TTL сессии, сек                        |
| `AUTH_ADMIN_SESSION_TTL`       | `3153600000`         | TTL токена `admin`                     |
| `AUTH_DEV_ADMIN_PASSWORD`      | `dev-admin-password` | Пароль `admin` в dev                   |
| `AUTH_COOKIE_SECURE`           | `false`              | `Secure` у cookie                      |
| `AUTH_THROTTLE_MAX_ATTEMPTS`   | `5`                  | Попыток входа в окне                   |
| `AUTH_THROTTLE_WINDOW_SECONDS` | `300`                | Окно throttle, сек                     |

Ключ подписи (`auth.signing-key-b64`, env `AUTH_SIGNING_KEY_B64`) — секрет
из `.env`, генерируется один раз скриптом `infra/scripts/ensure-env.sh` и в
Git не входит.

## Защита входа

`LoginThrottle` ограничивает неудачные попытки: `5` на комбинацию логин+IP за
`300` секунд, затем `429 TOO_MANY_ATTEMPTS`. Состояние in-memory и теряется при
рестарте. Пароль для несуществующего логина всё равно проверяется по dummy-hash
— время ответа не выдаёт наличие аккаунта.

## Наблюдаемость

`ObsFilter` пишет метрики `http.requests` и `http.request.duration`. В качестве
label `path` берётся шаблон маршрута (`BEST_MATCHING_PATTERN_ATTRIBUTE`), а не
сырой URI — кардинальность метрик ограничена, 404 по мусорным путям складываются
в `__unmatched__`. Каждый ответ несёт `X-Request-ID`.

## Отказы

- Неверные креденшелы → `401 UNAUTHORIZED`.
- Превышен throttle → `429 TOO_MANY_ATTEMPTS`.
- Нет `auth.signing-key-b64` → старт падает (fail closed).

## Тесты

Юнит-тестов нет. Сборка и `mvn test` гоняются в CI.

## Ссылки

`contracts/openapi/openapi.yaml`, `docs/infrastructure/docker-compose.md`.
