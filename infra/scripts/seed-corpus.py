"""Заливает демо-корпус через внутренний API documents (docker-compose).

Прямой адрес 127.0.0.1:18081 (публикуется только на loopback) и заголовок
X-Internal-Token, как его ждут /internal/* эндпоинты documents.
"""

import json
import os
import pathlib
import sys
import time
import urllib.error
import urllib.request

CORPUS = pathlib.Path(__file__).resolve().parent / "data" / "corpus.json"
ROOT = pathlib.Path(__file__).resolve().parents[2]
URL = os.environ.get("DOCUMENTS_SEED_URL", "http://127.0.0.1:18081") + "/internal/documents"
DEADLINE_SECONDS = 120


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


def post(payload: dict, timeout: int = 60) -> None:
    headers = {"Content-Type": "application/json"}
    if TOKEN:
        headers["X-Internal-Token"] = TOKEN
    request = urllib.request.Request(
        URL, data=json.dumps(payload).encode(), headers=headers, method="POST"
    )
    with urllib.request.urlopen(request, timeout=timeout) as response:
        response.read()


def main() -> None:
    documents = json.loads(CORPUS.read_text(encoding="utf-8"))["documents"]
    deadline = time.monotonic() + DEADLINE_SECONDS
    for document in documents:
        payload = {
            "external_id": document["id"],
            "title": document["title"],
            "audience": document["audience"],
            "section": document["section"],
            "text": document["text"],
        }
        while True:
            try:
                post(payload)
                break
            except (urllib.error.URLError, TimeoutError, ConnectionError, OSError) as exc:
                # Стенд ещё поднимается или documents перезапустился.
                if time.monotonic() > deadline:
                    raise SystemExit(f">> корпус: прервано — {exc}") from exc
                time.sleep(2)
    print(f">> корпус: {len(documents)} документов")


if __name__ == "__main__":
    sys.exit(main())
