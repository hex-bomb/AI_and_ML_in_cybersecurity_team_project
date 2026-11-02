import type {
  AnswerRequest,
  AnswerResponse,
  SearchRequest,
  SearchResponse,
} from "@/shared/types/api"
import { request } from "./http"

export type ApiCallOptions = {
  /** Базовый адрес API (ТЗ 8.5, `api_url`). */
  apiUrl?: string
  signal?: AbortSignal
}

/**
 * Поиск по базе знаний.
 * Сессия живёт в httpOnly cookie, поэтому заголовка авторизации здесь нет.
 */
export function searchDocuments(
  body: SearchRequest,
  options: ApiCallOptions,
): Promise<SearchResponse> {
  return request<SearchResponse>("/search", {
    method: "POST",
    body,
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
    apiUrl: options.apiUrl,
    signal: options.signal,
  })
}
