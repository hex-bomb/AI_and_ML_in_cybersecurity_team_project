from dataclasses import dataclass
from typing import Any

import httpx
import jwt
from fastapi import Depends, Request

from app.clients import Clients
from app.db import Dialogs
from app.errors import AppError
from app.settings import DATABASE_URL, TOP_K
from app.usecases.ask import AskQuestion
from app.usecases.ask_file import AskAboutFile


@dataclass(frozen=True)
class Auth:
    """Проверенные claims и исходный токен: токен нужен для вызовов соседей."""

    claims: dict[str, Any]
    token: str


_clients = Clients()
_dialogs: Dialogs | None = None


def get_clients() -> Clients:
    return _clients


def get_dialogs() -> Dialogs:
    """Пул создаётся лениво: импорт модуля не должен ходить в сеть."""
    global _dialogs
    if _dialogs is None:
        _dialogs = Dialogs(DATABASE_URL)
    return _dialogs


def token_of(request: Request) -> str | None:
    cookie = request.cookies.get("vektor_session")
    if cookie:
        return cookie
    header = request.headers.get("Authorization", "")
    return header[7:] if header.startswith("Bearer ") else None


async def authenticated(request: Request, clients: Clients = Depends(get_clients)) -> Auth:
    """Проверить сессию.

    Отсутствие токена и неверная подпись — 401, недоступный auth — 503:
    «нет авторизации» и «сервис сломан» нельзя смешивать.
    """
    token = token_of(request)
    try:
        claims = await clients.verify(token)
    except jwt.PyJWTError as exc:
        raise AppError.unauthorized() from exc
    except (httpx.HTTPError, OSError, TimeoutError) as exc:
        raise AppError.auth_unavailable() from exc
    return Auth(claims=claims, token=token or "")


def ask_question(
    clients: Clients = Depends(get_clients),
    dialogs: Dialogs = Depends(get_dialogs),
) -> AskQuestion:
    return AskQuestion(clients, clients, dialogs, TOP_K)


def ask_about_file(clients: Clients = Depends(get_clients)) -> AskAboutFile:
    return AskAboutFile(clients, clients, clients, TOP_K)
