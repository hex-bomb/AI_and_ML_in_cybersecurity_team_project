import type {
  AnswerRequest,
  AnswerResponse,
  SearchRequest,
  SearchResponse,
} from "@/shared/types/api"
import { request } from "./http"

export type ApiCallOptions = {
  token: string
  /** Базовый адрес API (ТЗ 8.5, `api_url`). */
  apiUrl?: string
  signal?: AbortSignal
}

/**
 * Поиск по базе знаний.
 * На 401 вызывающий код один раз вызывает tokenProvider.invalidate(),
 * получает свежий токен и повторяет вызов — автоматических повторов здесь нет.
 */
export function searchDocuments(
  body: SearchRequest,
  options: ApiCallOptions,
): Promise<SearchResponse> {
  return request<SearchResponse>("/search", {
    method: "POST",
    body,
    token: options.token,
    apiUrl: options.apiUrl,
    signal: options.signal,
  })
}

/** Генерация ответа по базе знаний со ссылками на источники. */
export function generateAnswer(
  body: AnswerRequest,
  options: ApiCallOptions,
): Promise<AnswerResponse> {
  return request<AnswerResponse>("/answers", {
    method: "POST",
    body,
    token: options.token,
    apiUrl: options.apiUrl,
    signal: options.signal,
  })
}