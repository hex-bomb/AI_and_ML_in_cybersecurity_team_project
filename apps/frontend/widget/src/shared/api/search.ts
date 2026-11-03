import type { AnswerRequest, AnswerResponse } from "@/shared/types/api"
import { request } from "./http"

export type ApiCallOptions = {
  /** Базовый адрес API (ТЗ 8.5, `api_url`). */
  apiUrl?: string
  signal?: AbortSignal
}

/**
 * Генерация ответа по базе знаний со ссылками на источники.
 * Сессия живёт в httpOnly cookie, поэтому заголовка авторизации здесь нет.
 */
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
