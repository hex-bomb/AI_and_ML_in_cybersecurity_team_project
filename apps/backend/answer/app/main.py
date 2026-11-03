"""Сборка приложения: роуты, метрики, обработчики ошибок.

Здесь нет бизнес-логики: порядок «guard → поиск → LLM» живёт в сценариях
(``app/usecases``), транспорт только разбирает вход и переводит результат.
"""

import logging
import time
import uuid
from contextlib import asynccontextmanager
from typing import Any, Literal

from fastapi import Depends, FastAPI, File, Form, Request, UploadFile
from fastapi.responses import JSONResponse
from prometheus_client import Counter, Histogram, make_asgi_app
from pydantic import BaseModel, Field

from app.deps import Auth, ask_about_file, ask_question, authenticated, get_clients
from app.errors import AppError
from app.settings import MAX_UPLOAD
from app.usecases.ask import AskQuestion, Question
from app.usecases.ask_file import AskAboutFile, FileQuestion

log = logging.getLogger("answer")

ANSWERED = Counter("answer_total", "Answers by status", ["status"])
REQUESTS = Counter("http_requests_total", "HTTP requests", ["method", "path", "status"])
LATENCY = Histogram("http_request_duration_seconds", "Request duration", ["path"])


@asynccontextmanager
async def lifespan(_: FastAPI):
    yield
    await get_clients().close()


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


@app.exception_handler(AppError)
async def app_error(request: Request, exc: AppError) -> JSONResponse:
    error: dict[str, Any] = {"code": exc.code, "message": exc.message}
    if exc.details:
        error["details"] = exc.details
    return JSONResponse(
        status_code=exc.status,
        content={"error": error, "request_id": rid(request)},
    )


@app.exception_handler(Exception)
async def unhandled(request: Request, exc: Exception) -> JSONResponse:
    """Наружу — стабильный контракт ошибки, внутрь — стек в логе."""
    log.exception("unhandled error: %s", exc)
    return await app_error(request, AppError.internal())


@app.get("/healthz")
async def healthz() -> dict[str, str]:
    return {"status": "ok"}


@app.post("/api/v1/answers")
async def answers(
    request: Request,
    payload: AnswerRequest,
    auth: Auth = Depends(authenticated),
    ask: AskQuestion = Depends(ask_question),
) -> JSONResponse:
    if payload.kb_id != auth.claims.get("kb"):
        raise AppError.kb_not_found()
    if not auth.claims.get("scp"):
        raise AppError.forbidden()

    result = await ask(
        Question(
            kb_id=payload.kb_id,
            text=payload.question.strip(),
            history=tuple(
                f"{turn.role}: {' '.join(turn.text.split())}"
                for turn in payload.history
                if turn.text.strip()
            ),
            token=auth.token,
        )
    )
    return answer_response(result, request)


@app.post("/api/v1/answers/file")
async def answers_file(
    request: Request,
    file: UploadFile = File(...),
    question: str = Form(min_length=1, max_length=4000),
    auth: Auth = Depends(authenticated),
    ask: AskAboutFile = Depends(ask_about_file),
) -> JSONResponse:
    content = await file.read(MAX_UPLOAD + 1)
    if len(content) > MAX_UPLOAD:
        raise AppError.too_large()
    if not content:
        raise AppError.bad_request("пустой файл")

    result = await ask(
        FileQuestion(
            filename=file.filename or "",
            content=content,
            question=question,
        )
    )
    return answer_response(result, request)


def answer_response(result: dict[str, Any], request: Request) -> JSONResponse:
    ANSWERED.labels(result["status"]).inc()
    return JSONResponse(content={**result, "request_id": rid(request)})
