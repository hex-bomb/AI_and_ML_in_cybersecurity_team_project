import asyncio

from app.clients import BadRequest, Clients, Unavailable, _extract_failure


def test_extract_separates_bad_file_from_broken_config() -> None:
    # 400/413/415 — вина файла, клиенту честно 400.
    assert isinstance(_extract_failure(400), BadRequest)
    assert isinstance(_extract_failure(413), BadRequest)
    assert isinstance(_extract_failure(415), BadRequest)
    # 401/403 — неверный внутренний токен: это отказ зависимости, а не файла.
    assert isinstance(_extract_failure(401), Unavailable)
    assert isinstance(_extract_failure(403), Unavailable)
    assert isinstance(_extract_failure(500), Unavailable)


def test_llm_calls_are_bounded(monkeypatch) -> None:
    monkeypatch.setenv("LLM_BASE_URL", "http://llm.test/v1")
    monkeypatch.setenv("LLM_API_KEY", "key")
    monkeypatch.setenv("LLM_CONCURRENCY", "2")
    monkeypatch.setenv("JWKS_URL", "http://auth.test/.well-known/jwks.json")
    clients = Clients()

    active = 0
    peak = 0

    async def fake_post(payload, headers):
        nonlocal active, peak
        active += 1
        peak = max(peak, active)
        await asyncio.sleep(0.01)
        active -= 1

        class Response:
            status_code = 200

            @staticmethod
            def json():
                return {"choices": [{"message": {"content": "ok"}}]}

        return Response()

    clients._post_chat = fake_post

    async def run() -> None:
        answers = await asyncio.gather(*(clients.complete("s", "u") for _ in range(8)))
        await clients.close()
        assert answers == ["ok"] * 8

    asyncio.run(run())
    assert peak <= 2, f"в LLM ушло {peak} одновременных запросов при лимите 2"
