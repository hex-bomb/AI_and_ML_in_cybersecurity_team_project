"""GigaChat: обмен Authorization key на access_token, без прокси.

Контракт — https://developers.sber.ru/docs/files/openapi/gigachat/api.yml:

- ключ авторизации (base64 от ``ClientID:ClientSecret``) обменивается на
  ``access_token``, который живёт 30 минут; обмен не чаще 10 раз в секунду;
- chat-эндпоинт принимает ``Bearer <access_token>`` и отвечает
  OpenAI-совместимым JSON (``choices[0].message.content``);
- заголовок User-Agent рекомендован спецификацией.

На живом стенде обмен ключа и запрос к GigaChat-2 отвечают 200.
"""

import time
import uuid

import httpx

OAUTH_URL = "https://ngw.devices.sberbank.ru:9443/api/v2/oauth"
BASE_URL = "https://api.giga.chat/v1"
# «Голого» GigaChat на api.giga.chat нет — фактический список: GET /v1/models.
DEFAULT_MODEL = "GigaChat-2"
DEFAULT_SCOPE = "GIGACHAT_API_PERS"
USER_AGENT = "vektor-answer/1.0"
# Токен обновляется за 2 минуты до истечения, чтобы 401 не дошёл до пользователя.
REFRESH_MARGIN_SECONDS = 120


class GigaChatAuthError(Exception):
    """Обмен ключа на токен не удался: наружу идёт 503 LLM_UNAVAILABLE."""


class GigaChatAuth:
    """Кэш access_token поверх Authorization key."""

    def __init__(self, http: httpx.AsyncClient, key: str, scope: str = DEFAULT_SCOPE) -> None:
        self._http = http
        self._key = key
        self._scope = scope
        # Готовый access_token (JWT) принимается как есть, без обмена.
        self.static_token = key.startswith("eyJ")
        self._token: str | None = None
        self._expires_at_ms = 0

    async def bearer(self, force_refresh: bool = False) -> str:
        if self.static_token:
            return self._key
        if not force_refresh and self._fresh():
            return self._token or ""
        return await self._exchange()

    def _fresh(self) -> bool:
        if not self._token:
            return False
        now_ms = time.time() * 1000
        return now_ms < self._expires_at_ms - REFRESH_MARGIN_SECONDS * 1000

    async def _exchange(self) -> str:
        try:
            response = await self._http.post(
                OAUTH_URL,
                data={"scope": self._scope},
                headers={
                    "Authorization": f"Basic {self._key}",
                    "Accept": "application/json",
                    "Content-Type": "application/x-www-form-urlencoded",
                    "RqUID": str(uuid.uuid4()),
                },
                timeout=30.0,
            )
        except httpx.HTTPError as exc:
            raise GigaChatAuthError(f"gigachat oauth: {exc}") from exc
        if response.status_code != 200:
            raise GigaChatAuthError(f"gigachat oauth status {response.status_code}")

        payload = response.json()
        token = payload.get("access_token") or ""
        if not token:
            raise GigaChatAuthError("gigachat oauth: нет access_token")
        self._token = token
        self._expires_at_ms = self._parse_expiry(payload.get("expires_at"))
        return token

    @staticmethod
    def _parse_expiry(value: object) -> int:
        # Спека: unix timestamp в миллисекундах; страхуемся от секунд.
        try:
            raw = int(value)  # type: ignore[arg-type]  # expires_at бывает числом или строкой
        except (TypeError, ValueError):
            return int(time.time() * 1000) + 25 * 60 * 1000
        if raw < 10**12:
            raw *= 1000
        return raw
