SHELL := /bin/bash

.PHONY: up down dev-up dev-down env ps logs smoke test lint clean

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

test:
	./scripts/test.sh

lint:
	./scripts/lint.sh

# Сброс данных стенда; .env (секреты) сохраняется.
clean:
	docker compose down -v
