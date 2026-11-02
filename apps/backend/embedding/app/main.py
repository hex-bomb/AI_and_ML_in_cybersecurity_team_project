"""Модель e5 за HTTP: батчинг, прогрев, корректная деградация без весов."""

import asyncio
import os
from pathlib import Path
from typing import Literal

import numpy as np
from fastapi import FastAPI
from prometheus_client import Counter, Histogram, make_asgi_app
from pydantic import BaseModel, Field

MODEL_PATH = Path(os.environ.get("MODEL_PATH", "model.onnx"))
TOKENIZER_PATH = Path(os.environ.get("TOKENIZER_PATH", "tokenizer.json"))
BATCH_SIZE = int(os.environ.get("BATCH_SIZE", "8"))
CONCURRENCY = int(os.environ.get("CONCURRENCY", "2"))
THREADS = int(os.environ.get("THREADS", "2"))
MAX_TOKENS = int(os.environ.get("MAX_TOKENS", "512"))

QUERY_PREFIX = "query: "
PASSAGE_PREFIX = "passage: "

EMBEDDED = Counter("embedding_texts_total", "Texts embedded", ["kind"])
BATCH = Histogram("embedding_batch_size", "Batch size", buckets=(1, 2, 4, 8, 16, 32, 64))


class Model:
    """ONNX-сессия e5: загрузка один раз, кодирование в потоке."""

    def __init__(self) -> None:
        self._session = None
        self._tokenizer = None
        self._lock = asyncio.Lock()
        self._sem = asyncio.Semaphore(CONCURRENCY)
        self._tried = False
        self.reason = ""

    @property
    def ready(self) -> bool:
        return self._session is not None

    async def ensure(self) -> bool:
        if self._session is not None:
            return True
        async with self._lock:
            if self._session is not None:
                return True
            if self._tried:
                return False
            self._tried = True
            return await asyncio.to_thread(self._load)

    async def encode(self, texts: list[str]) -> list[list[float] | None]:
        async with self._sem:
            return await asyncio.to_thread(self._encode_sync, texts)

    def _load(self) -> bool:
        try:
            import onnxruntime as ort
            from tokenizers import Tokenizer
        except ImportError as exc:
            self.reason = f"нет зависимостей модели: {exc}"
            return False
        if not MODEL_PATH.is_file() or not TOKENIZER_PATH.is_file():
            self.reason = f"нет файлов модели: {MODEL_PATH}"
            return False
        try:
            options = ort.SessionOptions()
            options.intra_op_num_threads = THREADS
            tokenizer = Tokenizer.from_file(str(TOKENIZER_PATH))
            tokenizer.enable_truncation(max_length=MAX_TOKENS)
            tokenizer.no_padding()
            self._session = ort.InferenceSession(
                str(MODEL_PATH),
                sess_options=options,
                providers=["CPUExecutionProvider"],
            )
            self._tokenizer = tokenizer
        except Exception as exc:
            self.reason = f"модель не загрузилась: {exc}"
            return False
        return True

    def _encode_sync(self, texts: list[str]) -> list[list[float] | None]:
        out: list[list[float] | None] = []
        for text in texts:
            ids = self._tokenizer.encode(text).ids
            if not ids:
                out.append(None)
                continue
            mask = np.ones((1, len(ids)), dtype=np.int64)
            feed = {
                "input_ids": np.array([ids], dtype=np.int64),
                "attention_mask": mask,
                "token_type_ids": np.zeros((1, len(ids)), dtype=np.int64),
            }
            hidden = self._session.run(None, feed)[0][0]
            pooled = (hidden * mask[0][:, None]).sum(axis=0) / float(mask.sum())
            norm = float(np.linalg.norm(pooled)) or 1.0
            out.append((pooled / norm).astype(np.float32).tolist())
        return out


model = Model()


class EmbedRequest(BaseModel):
    texts: list[str] = Field(min_length=1, max_length=256)
    kind: Literal["query", "passage"] = "passage"


app = FastAPI(title="vektor-embedding")
app.mount("/metrics", make_asgi_app())


@app.get("/healthz")
async def healthz() -> dict[str, object]:
    await model.ensure()
    return {"status": "ok" if model.ready else "degraded", "reason": model.reason}


@app.post("/embed")
async def embed(payload: EmbedRequest) -> dict[str, object]:
    if not await model.ensure():
        return {
            "vectors": [None] * len(payload.texts),
            "degraded": True,
            "reason": model.reason,
        }

    prefix = QUERY_PREFIX if payload.kind == "query" else PASSAGE_PREFIX
    texts = [prefix + text for text in payload.texts]

    BATCH.observe(len(texts))
    EMBEDDED.labels(payload.kind).inc(len(texts))

    vectors: list[list[float] | None] = []
    for start in range(0, len(texts), BATCH_SIZE):
        vectors.extend(await model.encode(texts[start : start + BATCH_SIZE]))
    return {"vectors": vectors}
