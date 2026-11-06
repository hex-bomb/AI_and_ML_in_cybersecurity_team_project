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

/**
 * Контекст страницы, собранный хостом по действию пользователя. Сервер считает
 * его недоверенным: обрезает поля и не меняет от него доступ.
 */
export type PageContext = {
  url?: string
  title?: string
  content_key?: string
  version_hint?: string
  section_key?: string
  selection?: {
    text: string
    prefix?: string
    suffix?: string
  }
}

export type AnswerRequest = {
  question: string
  /** Пусто — сервер создаст диалог и вернёт его в ответе. */
  conversation_id?: string
  /** Ключ намерения: повтор с тем же ключом не создаёт вторую реплику. */
  client_message_id?: string
  /** Контекст страницы: подсказка для поиска, не разрешение на чтение. */
  page_context?: PageContext
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
  conversation_id: string
  turn_id: string
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
