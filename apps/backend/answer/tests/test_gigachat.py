import asyncio
import time

import httpx
from app.gigachat import (
    OAUTH_URL,
    GigaChatAuth,
    GigaChatAuthError,
)


def run(coro):
    return asyncio.run(coro)


class FakeOAuth:
    """Считает вызовы oauth и отдаёт токен с заданным сроком жизни."""

    def __init__(self, expires_at: int, status: int = 200):
        self.calls = 0
        self.expires_at = expires_at
        self.status = status
        self.last_headers: dict[str, str] = {}
        self.last_data: dict[str, str] = {}

    def handler(self, request: httpx.Request) -> httpx.Response:
        assert str(request.url) == OAUTH_URL
        self.calls += 1
        self.last_headers = dict(request.headers)
        self.last_data = dict(pair.split("=", 1) for pair in request.content.decode().split("&"))
        if self.status != 200:
            return httpx.Response(self.status, json={"message": "nope"})
        return httpx.Response(200, json={"access_token": "tok", "expires_at": self.expires_at})


def make_auth(fake: FakeOAuth) -> tuple[GigaChatAuth, httpx.AsyncClient]:
    client = httpx.AsyncClient(transport=httpx.MockTransport(fake.handler))
    return GigaChatAuth(client, "aWQ6c2VjcmV0"), client


def test_exchanges_key_and_caches_token() -> None:
    fake = FakeOAuth(expires_at=int(time.time() * 1000) + 60 * 60 * 1000)
    auth, client = make_auth(fake)
    try:
        assert run(auth.bearer()) == "tok"
        assert run(auth.bearer()) == "tok"  # из кэша, без второго обмена
    finally:
        run(client.aclose())
    assert fake.calls == 1
    # Ключ авторизации уходит в Basic, scope — в тело (по спеке).
    assert fake.last_headers["authorization"].lower().startswith("basic ")
    assert fake.last_data["scope"] == "GIGACHAT_API_PERS"


def test_refreshes_when_token_near_expiry() -> None:
    # Живёт 30 секунд — меньше margin 120s, значит сразу считается устаревшим.
    fake = FakeOAuth(expires_at=int(time.time() * 1000) + 30 * 1000)
    auth, client = make_auth(fake)
    try:
        run(auth.bearer())
        run(auth.bearer())
    finally:
        run(client.aclose())
    assert fake.calls == 2


def test_expiry_in_seconds_is_accepted() -> None:
    fake = FakeOAuth(expires_at=int(time.time()) + 3600)  # секунды, не миллисекунды
    auth, client = make_auth(fake)
    try:
        run(auth.bearer())
        assert run(auth.bearer()) == "tok"  # кэш свежий после пересчёта
    finally:
        run(client.aclose())
    assert fake.calls == 1


def test_static_jwt_passes_through_without_http() -> None:
    auth = GigaChatAuth(http=None, key="eyJhbGciOiJSUzI1NiJ9.sig")  # type: ignore[arg-type]
    assert auth.static_token
    assert run(auth.bearer()) == "eyJhbGciOiJSUzI1NiJ9.sig"
    assert run(auth.bearer(force_refresh=True)) == "eyJhbGciOiJSUzI1NiJ9.sig"


def test_oauth_failure_raises() -> None:
    fake = FakeOAuth(expires_at=0, status=401)
    auth, client = make_auth(fake)
    try:
        try:
            run(auth.bearer())
            raise AssertionError("ожидали GigaChatAuthError")
        except GigaChatAuthError as exc:
            assert "401" in str(exc)
    finally:
        run(client.aclose())
