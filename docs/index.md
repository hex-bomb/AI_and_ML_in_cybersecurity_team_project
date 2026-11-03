# Документация «ВЕКТОР»

Микросервисная реализация демо: чат по базе знаний с ролями, гибридным поиском
и записью документов через диалог.

**Что это.** Демонстрационный стенд: данные компании вымышлены, архитектура
настоящая — сервисы на Go, Java и Python, Kafka, Prometheus и Grafana,
развёртывание локальное — docker-compose и `.env`.

## Как читать

| Что нужно                                         | Куда идти                                                              |
| ------------------------------------------------- | ---------------------------------------------------------------------- |
| Поднять стенд и проверить, что всё живо           | [environments/local-development.md](environments/local-development.md) |
| Посмотреть эндпоинты, лимиты и коды ошибок        | [reference/api.md](reference/api.md)                                   |
| Понять, почему сервисы разделены именно так       | [architecture/overview.md](architecture/overview.md)                   |
| Добавить эндпоинт, провайдера, событие или сервис | [architecture/extensibility.md](architecture/extensibility.md)         |
| Найти значение термина                            | [glossary.md](glossary.md)                                             |

## Состав

| Каталог                          | Что внутри                                                                                      |
| -------------------------------- | ----------------------------------------------------------------------------------------------- |
| `apps/backend/`                  | Сервисы: `auth`, `documents` (Java), `retrieval` (Go), `answer`, `embedding`, `ingest` (Python) |
| `apps/frontend/`                 | `web-app` — лендинг, `widget` — чат в iframe (React, Vite, npm workspaces)                      |
| `apps/backend/embedding/models/` | Веса модели эмбеддингов. В git не входят                                                        |
| `contracts/`                     | OpenAPI и коллекция Postman                                                                     |
| `infra/`                         | Конфиги развёртывания: router, Prometheus, Grafana, скрипты и демо-корпус                       |
| `docs/`                          | Эта документация                                                                                |

## Статус

Работает: вход по cookie, поиск, ответ по базе с проверенными цитатами, загрузка
документов с асинхронной индексацией, ответ по приложенному файлу, метрики и
дашборд.

Не сделано осознанно: DLQ и outbox (индексация чинится повторной публикацией),
TLS на входном router, удаление документов.
Подробности — [architecture/overview.md](architecture/overview.md).

Проверка перед показом — [environments/local-development.md](environments/local-development.md).

## Как внести изменение

Ветка отводится от `develop`, PR идёт в `develop`. Шаблон описания подставится
сам; его чек-лист повторяет [«Перед PR»](architecture/extensibility.md#перед-pr).
Ревьюер назначается автоматически, если PR открыл не владелец репозитория:
себя GitHub ревьюером не назначает.
