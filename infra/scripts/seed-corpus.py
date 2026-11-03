"""Заливает демо-корпус через внутренний API documents (docker-compose).

Прямой адрес 127.0.0.1:18081 (публикуется только на loopback) и заголовок
X-Internal-Token, как его ждут /internal/* эндпоинты documents.

После загрузки документ не виден в поиске: индексатор должен отчитаться о
готовности, и только потом версию можно опубликовать. Скрипт ждёт готовности
и публикует — иначе корпус остался бы черновиками.
"""

import json
import os
import pathlib
import sys
import time
import urllib.error
import urllib.parse
import urllib.request

CORPUS = pathlib.Path(__file__).resolve().parent / "data" / "corpus.json"
ROOT = pathlib.Path(__file__).resolve().parents[2]
BASE = os.environ.get("DOCUMENTS_SEED_URL", "http://127.0.0.1:18081")
URL = BASE + "/internal/documents"
DEADLINE_SECONDS = 180
POLL_SECONDS = 2

# Корпус писан под старую модель аудиторий, схема знает каналы.
CHANNELS = {"public": "public", "employee": "internal"}


def token_from_env() -> str:
    if os.environ.get("DOCUMENTS_INTERNAL_TOKEN"):
        return os.environ["DOCUMENTS_INTERNAL_TOKEN"]
    # Запуск мимо make: достаём токен из .env вручную.
    env_file = ROOT / ".env"
    if env_file.exists():
        for line in env_file.read_text(encoding="utf-8").splitlines():
            if line.startswith("DOCUMENTS_INTERNAL_TOKEN="):
                return line.split("=", 1)[1].strip()
    return ""


TOKEN = token_from_env()


def call(url: str, payload: dict, timeout: int = 60) -> int:
    headers = {"Content-Type": "application/json"}
    if TOKEN:
        headers["X-Internal-Token"] = TOKEN
    request = urllib.request.Request(
        url, data=json.dumps(payload).encode(), headers=headers, method="POST"
    )
    try:
        with urllib.request.urlopen(request, timeout=timeout) as response:
            response.read()
            return response.status
    except urllib.error.HTTPError as error:
        return error.code


def post(payload: dict, timeout: int = 60) -> None:
    """Загрузка документа: ждём, пока сервис поднимется."""
    deadline = time.monotonic() + DEADLINE_SECONDS
    while True:
        status = call(URL, payload, timeout)
        if status < 400:
            return
        if time.monotonic() > deadline:
            raise SystemExit(f">> корпус: documents ответил {status}")
        time.sleep(POLL_SECONDS)


def publish(external_id: str) -> None:
    """Публикация возможна только после отчёта индексатора о готовности."""
    deadline = time.monotonic() + DEADLINE_SECONDS
    url = f"{BASE}/internal/publish/{urllib.parse.quote(external_id)}"
    while True:
        status = call(url, {})
        if status < 400:
            return
        # 400 — версия ещё не готова; 404 — документа пока нет. Оба ждут.
        if status not in (400, 404):
            raise SystemExit(f">> корпус: публикация {external_id} — {status}")
        if time.monotonic() > deadline:
            raise SystemExit(f">> корпус: {external_id} не готов к публикации")
        time.sleep(POLL_SECONDS)


def main() -> None:
    documents = json.loads(CORPUS.read_text(encoding="utf-8"))["documents"]
    for document in documents:
        payload = {
            "external_id": document["id"],
            "title": document["title"],
            "channel": CHANNELS.get(document.get("audience", "public"), "public"),
            "section": document["section"],
            "text": document["text"],
        }
        post(payload)

    for document in documents:
        publish(document["id"])

    print(f">> корпус: {len(documents)} документов опубликовано")


if __name__ == "__main__":
    sys.exit(main())
