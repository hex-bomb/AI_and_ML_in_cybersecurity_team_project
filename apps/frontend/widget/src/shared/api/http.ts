import type { ApiError } from "@/shared/types/api"

/** Все эндпоинты живут под общим префиксом версии API. */
const API_PREFIX = "/api/v1"

export type RequestOptions = {
  method?: "GET" | "POST" | "PATCH" | "DELETE"
  body?: unknown
  /**
   * Базовый адрес API. По умолчанию — тот же origin, что и страница: так
   * виджет работает за одним origin без прокси. Для внешнего API адрес
   * передаётся явно (контракт ТЗ 8.5, опция `apiUrl`).
   */
  apiUrl?: string | null
  /** Ключ идемпотентности для повторяемых POST-запросов. */
  idempotencyKey?: string
  signal?: AbortSignal
}

/** Любая ошибка HTTP-слоя. Повторов внутри нет — решает вызывающий код. */
export class ApiRequestError extends Error {
  readonly status: number
  readonly code: string
  readonly details?: Record<string, unknown>
  readonly requestId?: string

  constructor(init: {
    status: number
    code: string
    message: string
    details?: Record<string, unknown>
    requestId?: string
  }) {
    super(init.message)
    this.name = "ApiRequestError"
    this.status = init.status
    this.code = init.code
    this.details = init.details
    this.requestId = init.requestId
  }
}

/** Тексты для кодов, которых нет в справочнике. */
const FALLBACK_MESSAGE: Record<number, string> = {
  400: "Некорректный запрос",
  401: "Требуется авторизация",
  403: "Доступ запрещён",
  404: "Ресурс не найден",
  413: "Слишком большой запрос",
  422: "Запрос не прошёл валидацию",
  429: "Слишком много запросов",
  500: "Внутренняя ошибка сервера",
  503: "Сервис недоступен",
}

export async function request<T>(path: string, options: RequestOptions = {}): Promise<T> {
  const { method = "GET", body, idempotencyKey, signal } = options
  // Адрес задаёт виджет (контракт ТЗ 8.5); без него запрос идёт на текущий
  // origin, как при обычном развёртывании за одним доменом.
  const base = (options.apiUrl ?? API_PREFIX).replace(/\/+$/, "")

  // Генерируем на клиенте, чтобы можно было связать лог браузера с логом бэкенда.
  const requestId = crypto.randomUUID()

  const headers: Record<string, string> = {
    "Content-Type": "application/json",
    "X-Request-ID": requestId,
  }
  if (idempotencyKey) headers["Idempotency-Key"] = idempotencyKey

  let response: Response
  try {
    response = await fetch(`${base}${path}`, {
      method,
      headers,
      body: body === undefined ? undefined : JSON.stringify(body),
      // Сессия — JWT в httpOnly cookie: читать её из JS нельзя и не нужно,
      // браузер прикладывает её сам.
      credentials: "include",
      signal,
    })
  } catch (error) {
    // Отмену пользователем пропускаем как есть — её разбирает вызывающий код.
    if (signal?.aborted) throw error
    throw new ApiRequestError({
      status: 0,
      code: "network_error",
      message: "Сеть недоступна",
      requestId,
    })
  }

  const payload = await readJson(response)

  if (!response.ok) throw toApiError(response.status, payload, requestId)
  return payload as T
}

/** Тело может оказаться не-JSON (прокси, HTML-заглушка, 204) — тогда это null. */
async function readJson(response: Response): Promise<unknown> {
  const text = await response.text()
  if (!text) return null
  try {
    return JSON.parse(text) as unknown
  } catch {
    return null
  }
}

function toApiError(status: number, payload: unknown, requestId: string): ApiRequestError {
  const error = readError(payload)
  return new ApiRequestError({
    status,
    code: error?.code ?? `http_${status}`,
    message: error?.message ?? FALLBACK_MESSAGE[status] ?? `Ошибка запроса (${status})`,
    details: error?.details,
    requestId: readRequestId(payload) ?? requestId,
  })
}

function readError(payload: unknown): ApiError | null {
  if (typeof payload !== "object" || payload === null) return null
  const error = (payload as { error?: unknown }).error
  if (typeof error !== "object" || error === null) return null

  const { code, message, details } = error as Partial<ApiError>
  if (typeof code !== "string" || typeof message !== "string") return null

  return {
    code,
    message,
    details:
      typeof details === "object" && details !== null
        ? (details as Record<string, unknown>)
        : undefined,
  }
}

function readRequestId(payload: unknown): string | null {
  if (typeof payload !== "object" || payload === null) return null
  const requestId = (payload as { request_id?: unknown }).request_id
  return typeof requestId === "string" ? requestId : null
}
