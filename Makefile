SHELL := /bin/bash

.PHONY: up down dev-up dev-down env ps logs smoke test lint clean \
	doctor backup restore reindex retention

# Полный подъём: секреты в .env → сборка и запуск → корпус → smoke.
# --wait возвращается, когда healthchecks всех сервисов зелёные.
up: env
	docker compose up -d --build --wait
	python3 infra/scripts/seed-corpus.py
	./infra/scripts/smoke.sh

env:
	./infra/scripts/ensure-env.sh

down: dev-down

dev-up: up

dev-down:
	docker compose down

ps:
	docker compose ps

logs:
	docker compose logs -f $(SVC)

smoke:
	./infra/scripts/smoke.sh

# Проверка установки: конфигурация, базы, миграции, блобы, поиск, очередь.
doctor:
	./infra/scripts/doctor.sh

# Копия баз и оригиналов; путь можно передать: make backup DIR=backups/nightly
backup:
	./infra/scripts/backup.sh $(DIR)

# Восстановление заменяет данные: подтверждение спрашивается отдельно.
restore:
	./infra/scripts/restore.sh $(DIR)

# Пересобрать поисковый индекс из базы (после restore, смены модели, чистки ES).
reindex:
	./infra/scripts/reindex.sh

# Производные данные и (опционально) история диалогов.
retention:
	./infra/scripts/retention.sh

test:
	./scripts/test.sh

lint:
	./scripts/lint.sh

# Сброс данных стенда; .env (секреты) сохраняется.
clean:
	docker compose down -v
