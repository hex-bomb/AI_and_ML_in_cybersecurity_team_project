import logging
import os
import time
import uuid
from contextlib import asynccontextmanager
from typing import Any, Literal

import httpx
import jwt
from fastapi import FastAPI, File, Form, Request, UploadFile
from fastapi.responses import JSONResponse
from prometheus_client import Counter, Histogram, make_asgi_app
from pydantic import BaseModel, Field

from app.clients import BadRequest, Clients, Unavailable
from app.guard import blocked_reason
from app.rag import (
    NO_EVIDENCE,
    SYSTEM,
    build_citations,
    build_file_citations,
    build_prompt,
    parse_answer,
    pseudo_hits,
    rank,
    slices,
)

TOP_K = int(os.environ.get("TOP_K", "4"))
MAX_UPLOAD = int(os.environ.get("MAX_UPLOAD_BYTES", str(5 << 20)))

log = logging.getLogger("answer")

ANSWERED = Counter("answer_total", "Answers by status", ["status"])
REQUESTS = Counter("http_requests_total", "HTTP requests", ["method", "path", "status"])
LATENCY = Histogram("http_request_duration_seconds", "Request duration", ["path"])

clients = Clients()


@asynccontextmanager
async def lifespan(_: FastAPI):
    yield
    await clients.close()


app = FastAPI(title="vektor-answer", lifespan=lifespan)
app.mount("/metrics", make_asgi_app())


class Turn(BaseModel):
    role: Literal["user", "assistant"]
    text: str = Field(min_length=1, max_length=1000)


class AnswerRequest(BaseModel):
    kb_id: str = Field(min_length=1, max_length=64)
    question: str = Field(min_length=1, max_length=4000)
    history: list[Turn] = Field(default_factory=list, max_length=20)


def rid(request: Request) -> str:
    return getattr(request.state, "rid", "")


def token_of(request: Request) -> str | None:
    cookie = request.cookies.get("vektor_session")
    if cookie:
        return cookie
    header = request.headers.get("Authorization", "")
    return header[7:] if header.startswith("Bearer ") else None


def error(
    status: int,
    code: str,
    message: str,
    request_id: str,
    details: dict[str, Any] | None = None,
) -> JSONResponse:
    body: dict[str, Any] = {"error": {"code": code, "message": message}}
    if details:
        body["error"]["details"] = details
    body["request_id"] = request_id
    return JSONResponse(status_code=status, content=body)


@app.middleware("http")
async def observe(request: Request, call_next):
    incoming = request.headers.get("X-Request-ID", "")
    request.state.rid = incoming if incoming and len(incoming) <= 128 else uuid.uuid4().hex
    started = time.perf_counter()
    response = None
    try:
        response = await call_next(request)
        return response
    finally:
        # Метрики и request_id должны появиться и на 500: иначе отказ
        # обработчика не виден ни в Prometheus, ни в логах клиента.
        route = request.scope.get("route")
        path = getattr(route, "path", None) or "__unmatched__"
        status = response.status_code if response is not None else 500
        LATENCY.labels(path).observe(time.perf_counter() - started)
        REQUESTS.labels(request.method, path, str(status)).inc()
        if response is not None:
            response.headers["X-Request-ID"] = request.state.rid


@app.exception_handler(Exception)
async def unhandled(request: Request, exc: Exception) -> JSONResponse:
    """Наружу — стабильный контракт ошибки, внутрь — стек в логе."""
    log.exception("unhandled error: %s", exc)
    return error(500, "INTERNAL", "Внутренняя ошибка", rid(request))


@app.get("/healthz")
async def healthz() -> dict[str, str]:
    return {"status": "ok"}


@app.post("/api/v1/answers")
async def answers(request: Request, payload: AnswerRequest) -> JSONResponse:
    request_id = rid(request)
    token = token_of(request)
    try:
        claims = await clients.verify(token)
    except jwt.PyJWTError:
        return error(401, "UNAUTHORIZED", "Требуется авторизация", request_id)
    except (httpx.HTTPError, OSError, TimeoutError):
        # JWKS недоступен — это не «нет авторизации», а отказ зависимости.
        return error(503, "AUTH_UNAVAILABLE", "Сервис авторизации недоступен", request_id)

    if payload.kb_id != claims.get("kb"):
        return error(404, "KB_NOT_FOUND", "Неизвестная база знаний", request_id)
    if not claims.get("scp"):
        return error(403, "FORBIDDEN", "Доступ запрещён", request_id)

    reason = blocked_reason(payload.question)
    if reason:
        return error(
            403,
            "FORBIDDEN",
            "Запрос отклонён политикой безопасности",
            request_id,
            {"reason": reason},
        )

    history = [
        f"{turn.role}: {' '.join(turn.text.split())}"
        for turn in payload.history
        if turn.text.strip()
    ]

    try:
        hits = await clients.search(payload.kb_id, payload.question.strip(), TOP_K, token or "")
    except Unavailable:
        return error(503, "SEARCH_UNAVAILABLE", "Поиск недоступен", request_id)

    if not hits:
        ANSWERED.labels("no_evidence").inc()
        return JSONResponse(
            status_code=200,
            content={
                "status": "no_evidence",
                "answer": NO_EVIDENCE,
                "citations": [],
                "request_id": request_id,
            },
        )

    try:
        raw = await clients.complete(SYSTEM, build_prompt(payload.question, history, hits))
    except Unavailable:
        return error(503, "LLM_UNAVAILABLE", "Генерация ответа недоступна", request_id)

    text, numbers = parse_answer(raw)
    if not text:
        return error(502, "LLM_BAD_OUTPUT", "Модель вернула пустой ответ", request_id)

    citations = build_citations(numbers or tuple(range(1, min(len(hits), 3) + 1)), hits)
    ANSWERED.labels("ok").inc()
    return JSONResponse(
        status_code=200,
        content={
            "status": "ok",
            "answer": text,
            "citations": citations,
            "request_id": request_id,
        },
    )


@app.post("/api/v1/answers/file")
async def answers_file(
    request: Request,
    file: UploadFile = File(...),
    question: str = Form(min_length=1, max_length=4000),
) -> JSONResponse:
    request_id = rid(request)
    try:
        await clients.verify(token_of(request))
    except jwt.PyJWTError:
        return error(401, "UNAUTHORIZED", "Требуется авторизация", request_id)
    except (httpx.HTTPError, OSError, TimeoutError):
        return error(503, "AUTH_UNAVAILABLE", "Сервис авторизации недоступен", request_id)

    reason = blocked_reason(question)
    if reason:
        return error(
            403,
            "FORBIDDEN",
            "Запрос отклонён политикой безопасности",
            request_id,
            {"reason": reason},
        )

    content = await file.read(MAX_UPLOAD + 1)
    if len(content) > MAX_UPLOAD:
        return error(413, "PAYLOAD_TOO_LARGE", "Файл больше лимита", request_id)
    if not content:
        return error(400, "INVALID_REQUEST", "пустой файл", request_id)

    try:
        text = await clients.extract(file.filename or "", content)
        texts = slices(text)
        vectors = await clients.embed(texts, "passage")
        query = (await clients.embed([question], "query"))[0]
    except BadRequest:
        return error(400, "INVALID_REQUEST", "Файл не удалось разобрать", request_id)
    except Unavailable:
        return error(503, "SERVICE_UNAVAILABLE", "Сервис недоступен", request_id)

    hits = pseudo_hits(file.filename or "", rank(texts, vectors, query, TOP_K))
    try:
        raw = await clients.complete(SYSTEM, build_prompt(question, [], hits))
    except Unavailable:
        return error(503, "LLM_UNAVAILABLE", "Генерация ответа недоступна", request_id)

    text, numbers = parse_answer(raw)
    if not text:
        return error(502, "LLM_BAD_OUTPUT", "Модель вернула пустой ответ", request_id)

    citations = build_file_citations(numbers or tuple(range(1, min(len(hits), 3) + 1)), hits)
    ANSWERED.labels("ok").inc()
    return JSONResponse(
        status_code=200,
        content={
            "status": "ok",
            "answer": text,
            "citations": citations,
            "request_id": request_id,
        },
    )
