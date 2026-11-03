"""Тесты транспорта: конверт ошибки, коды статусов, request_id.

Бизнес-логика здесь не проверяется — только перевод ``AppError`` в ответ
и разбор входа.
"""

import jwt
import pytest
from app.deps import Auth, ask_about_file, ask_question, authenticated, get_clients
from app.main import app
from fastapi.testclient import TestClient


class RejectingClient:
    async def verify(self, token):
        raise jwt.InvalidTokenError("bad token")


class StubAsk:
    def __init__(self, result=None) -> None:
        self.result = result or {"status": "no_evidence", "answer": "нет", "citations": []}
        self.calls = 0
        self.last = None

    async def __call__(self, ask):
        self.calls += 1
        self.last = ask
        return self.result


@pytest.fixture(autouse=True)
def clean_overrides():
    yield
    app.dependency_overrides.clear()


def auth(claims=None, session="session") -> Auth:
    return Auth(claims=claims or {"scp": ["public"]}, token=session)


def test_missing_token_is_401_with_envelope() -> None:
    app.dependency_overrides[get_clients] = RejectingClient

    response = TestClient(app).post("/api/v1/answers", json={"question": "вопрос"})
    body = response.json()

    assert response.status_code == 401
    assert body["error"]["code"] == "UNAUTHORIZED"
    assert body["request_id"]


def test_answer_adds_request_id_and_status() -> None:
    app.dependency_overrides[authenticated] = lambda: auth()
    app.dependency_overrides[ask_question] = lambda: StubAsk(
        {"status": "ok", "answer": "ответ", "citations": []}
    )

    response = TestClient(app).post(
        "/api/v1/answers",
        json={"question": "вопрос"},
        headers={"X-Request-ID": "rid-1"},
    )
    body = response.json()

    assert response.status_code == 200
    assert body["status"] == "ok"
    assert body["request_id"] == "rid-1"
    assert response.headers["X-Request-ID"] == "rid-1"


def test_oversized_file_is_413(monkeypatch) -> None:
    monkeypatch.setattr("app.main.MAX_UPLOAD", 4)
    app.dependency_overrides[authenticated] = lambda: auth()

    response = TestClient(app).post(
        "/api/v1/answers/file",
        files={"file": ("a.txt", b"12345")},
        data={"question": "что тут"},
    )

    assert response.status_code == 413
    assert response.json()["error"]["code"] == "PAYLOAD_TOO_LARGE"


def test_empty_file_is_400(monkeypatch) -> None:
    monkeypatch.setattr("app.main.MAX_UPLOAD", 4)
    app.dependency_overrides[authenticated] = lambda: auth()

    response = TestClient(app).post(
        "/api/v1/answers/file",
        files={"file": ("a.txt", b"")},
        data={"question": "что тут"},
    )

    assert response.status_code == 400
    assert response.json()["error"]["code"] == "INVALID_REQUEST"


def test_file_answer_uses_file_usecase(monkeypatch) -> None:
    monkeypatch.setattr("app.main.MAX_UPLOAD", 1024)
    stub = StubAsk({"status": "ok", "answer": "из файла", "citations": []})
    app.dependency_overrides[authenticated] = lambda: auth()
    app.dependency_overrides[ask_about_file] = lambda: stub

    response = TestClient(app).post(
        "/api/v1/answers/file",
        files={"file": ("a.txt", b"text")},
        data={"question": "что тут"},
    )

    assert response.status_code == 200
    assert response.json()["answer"] == "из файла"
    assert stub.calls == 1


def test_answer_passes_conversation_and_message_id() -> None:
    stub = StubAsk(
        {
            "status": "ok",
            "answer": "ответ",
            "citations": [],
            "conversation_id": "conv-1",
            "turn_id": "turn-1",
        }
    )
    app.dependency_overrides[authenticated] = lambda: auth(
        {"sub": "user-1", "scp": ["public", "employee"]}
    )
    app.dependency_overrides[ask_question] = lambda: stub

    response = TestClient(app).post(
        "/api/v1/answers",
        json={
            "question": "вопрос",
            "conversation_id": "6a1c1f3a-2f5a-4b4a-9a8e-0d4b1f0f4b7c",
            "client_message_id": "msg-1",
        },
    )
    body = response.json()

    assert response.status_code == 200
    assert body["conversation_id"] == "conv-1"
    assert body["turn_id"] == "turn-1"
    assert stub.last.client_message_id == "msg-1"
    assert stub.last.conversation_id is not None
    assert stub.last.subject == "user-1"
    assert stub.last.channel == "internal"
