import asyncio
import os
from typing import Any

import httpx
import jwt
from jwt import PyJWKClient

from app.errors import BadRequest, Unavailable
from app.gigachat import (
    BASE_URL as GIGACHAT_BASE_URL,
)
from app.gigachat import (
    DEFAULT_MODEL as GIGACHAT_MODEL,
)
from app.gigachat import (
    DEFAULT_SCOPE,
    GigaChatAuth,
    GigaChatAuthError,
)
from app.gigachat import (
    USER_AGENT as GIGACHAT_USER_AGENT,
)


def env(name: str, default: str = "") -> str:
    return os.environ.get(name, default)


# documents отвечает 400/413/415 на сам файл, а 401/403 — на токен.
# Разделяем эти случаи: иначе поломка конфигурации выглядит как «файл не разобран».
_DOCUMENTS_BAD_INPUT = frozenset({400, 413, 415})


def _extract_failure(status: int) -> Exception:
    if status in _DOCUMENTS_BAD_INPUT:
        return BadRequest(f"documents {status}")
    return Unavailable(f"documents {status}")


class Clients:
    def __init__(self) -> None:
        self.retrieval_url = env("RETRIEVAL_URL", "http://localhost:8080").rstrip("/")
        self.embedding_url = env("EMBEDDING_URL", "http://localhost:8000").rstrip("/")
        self.documents_url = env("DOCUMENTS_URL", "http://localhost:8080").rstrip("/")
        self.llm_timeout = float(env("LLM_TIMEOUT", "90"))
        self.internal_token = env("DOCUMENTS_INTERNAL_TOKEN")
        self._jwks = PyJWKClient(env("JWKS_URL"), cache_keys=True)
        self._http = httpx.AsyncClient(timeout=httpx.Timeout(30.0))
        # LLM — самая дорогая и самая медленная зависимость: без предела
        # всплеск запросов упирается в лимиты провайдера и в память процесса.
        self.llm_concurrency = max(1, int(env("LLM_CONCURRENCY", "8")))
        self._llm_sem = asyncio.Semaphore(self.llm_concurrency)

        # Нативный режим GigaChat: LLM_API_KEY — Authorization key,
        # answer сам обменивает его на 30-минутный access_token (без прокси).
        gigachat_mode = env("LLM_PROVIDER").lower() == "gigachat"
        self.gigachat: GigaChatAuth | None = None
        if gigachat_mode:
            self.gigachat = GigaChatAuth(
                self._http, env("LLM_API_KEY"), env("GIGACHAT_SCOPE", DEFAULT_SCOPE)
            )

        self.llm_base_url = env("LLM_BASE_URL", GIGACHAT_BASE_URL if gigachat_mode else "").rstrip(
            "/"
        )
        self.llm_api_key = env("LLM_API_KEY", "")
        self.llm_model = env("LLM_MODEL", GIGACHAT_MODEL if gigachat_mode else "gpt-4o-mini")

    async def close(self) -> None:
        await self._http.aclose()

    async def verify(self, token: str | None) -> dict[str, Any]:
        """Проверить подпись JWT по JWKS сервиса auth.

        Parameters
        ----------
        token : str | None
            Значение cookie или bearer-токен.

        Returns
        -------
        dict
            Claims: sub, role, scp, tenant, kb.
        """
        if not token:
            raise jwt.InvalidTokenError("no token")
        key = await asyncio.to_thread(self._jwks.get_signing_key_from_jwt, token)
        return jwt.decode(token, key.key, algorithms=["RS256"], options={"require": ["exp"]})

    async def search(self, query: str, top_k: int, token: str) -> list[dict[str, Any]]:
        response = await self._http.post(
            f"{self.retrieval_url}/api/v1/search",
            json={"query": query, "top_k": top_k},
            headers={"Authorization": f"Bearer {token}"},
        )
        if response.status_code != 200:
            raise Unavailable(f"retrieval {response.status_code}")
        return response.json()["hits"]

    async def embed(self, texts: list[str], kind: str) -> list[list[float] | None]:
        response = await self._http.post(
            f"{self.embedding_url}/embed", json={"texts": texts, "kind": kind}
        )
        if response.status_code != 200:
            raise Unavailable(f"embedding {response.status_code}")
        return response.json()["vectors"]

    async def extract(self, filename: str, content: bytes) -> str:
        """Разбор файла живёт в documents: PDFBox и POI не должны блокировать event loop.

        Parameters
        ----------
        filename : str
            Имя файла: по расширению выбирается парсер.
        content : bytes
            Тело загруженного файла.

        Returns
        -------
        str
            Извлечённый текст.
        """
        response = await self._http.post(
            f"{self.documents_url}/internal/extract",
            files={"file": (filename, content)},
            headers={"X-Internal-Token": self.internal_token} if self.internal_token else {},
        )
        if 400 <= response.status_code < 500:
            raise _extract_failure(response.status_code)
        if response.status_code != 200:
            raise Unavailable(f"documents {response.status_code}")
        return response.json()["text"]

    async def complete(self, system: str, user: str) -> str:
        if not self.llm_base_url:
            raise Unavailable("LLM_BASE_URL не задан")
        payload = {
            "model": self.llm_model,
            "temperature": 0.2,
            "messages": [
                {"role": "system", "content": system},
                {"role": "user", "content": user},
            ],
        }
        async with self._llm_sem:
            headers = {"Authorization": f"Bearer {self.llm_api_key}"}
            if self.gigachat is not None:
                headers = await self._gigachat_headers()

            response = await self._post_chat(payload, headers)
            # Токен мог истечь досрочно — обменять заново и повторить один раз.
            if (
                response.status_code == 401
                and self.gigachat is not None
                and not self.gigachat.static_token
            ):
                headers = await self._gigachat_headers(force_refresh=True)
                response = await self._post_chat(payload, headers)

        if response.status_code != 200:
            raise Unavailable(f"llm {response.status_code}")
        return response.json()["choices"][0]["message"]["content"]

    async def _gigachat_headers(self, force_refresh: bool = False) -> dict[str, str]:
        assert self.gigachat is not None
        try:
            token = await self.gigachat.bearer(force_refresh)
        except GigaChatAuthError as exc:
            raise Unavailable(str(exc)) from exc
        return {
            "Authorization": f"Bearer {token}",
            "User-Agent": GIGACHAT_USER_AGENT,
        }

    async def _post_chat(self, payload: dict[str, Any], headers: dict[str, str]) -> httpx.Response:
        return await self._http.post(
            f"{self.llm_base_url}/chat/completions",
            timeout=self.llm_timeout,
            headers=headers,
            json=payload,
        )
