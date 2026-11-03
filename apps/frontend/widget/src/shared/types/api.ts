// Контракт HTTP-слоя бэкенда (ТЗ 7.4 / 7.5).
// Все поля в snake_case — так их отдаёт сервер, переименования на клиенте запрещены.

/** Тело ошибки из конверта { error: { ... }, request_id }. */
export type ApiError = {
  code: string
  message: string
  details?: Record<string, unknown>
}

/** Где именно лежит фрагмент в документе. */
export type Locator = {
  block_ids: string[]
  section_path: string[]
  page_start: number | null
  page_end: number | null
}

export type SearchRequest = {
  query: string
  top_k?: number
}

export type SearchHit = {
  chunk_id: string
  document_id: string
  version_id: string
  title: string
  text: string
  locator: Locator
  source_url: string | null
  download_url: string
  score: number
}

export type SearchResponse = {
  status: "ok" | "no_results"
  hits: SearchHit[]
  request_id: string
}

export type AnswerTurn = {
  role: "user" | "assistant"
  text: string
}

export type AnswerRequest = {
  question: string
  /** Предыдущие реплики диалога: сервер берёт последние восемь. */
  history?: AnswerTurn[]
}

export type Citation = {
  number: number
  chunk_id: string
  document_id: string
  version_id: string
  title: string
  quote: string
  locator: Locator
  source_url: string | null
  download_url: string
}

export type AnswerResponse = {
  status: "ok" | "no_evidence"
  answer: string
  citations: Citation[]
  request_id: string
}

/** Демо-пользователь: `user` — клиент, `employee` — сотрудник. */
export type LoginUser = {
  id: string
  name: string
  role: "user" | "employee"
}

export type LoginRequest = {
  username: string
  password: string
}

/**
 * Ответ `/auth/login` и `/auth/me`. Токена в теле нет: JWT приходит
 * в httpOnly cookie и JS его не видит.
 */
export type MeResponse = {
  user: LoginUser
  request_id: string
}
