import { combine, createEffect, createEvent, createStore, sample } from "effector"

import { mockAnswer } from "@/shared/api/dev-mock"
import { ApiRequestError } from "@/shared/api/http"
import { generateAnswer } from "@/shared/api/search"
import {
  appendMessage,
  createChat,
  deleteChat,
  getMessages,
  listChats,
} from "@/shared/db/chats"
import type { AnswerResponse, Citation } from "@/shared/types/api"
import type { Chat, ChatMessage, ChatStatus } from "@/shared/types/chat"

/* ── конфигурация виджета ──────────────────────────────────── */

export type Config = {
  apiUrl: string
  kbId: string
  token: () => Promise<string>
  invalidateToken: () => void
  useMock: boolean
  onOpenSource?: (citation: Citation) => void
}

/** Ставится снаружи один раз на mount — см. widgets/mount.tsx. */
export const $config = createStore<Config>({
  apiUrl: "/api/v1",
  kbId: "",
  token: async () => "",
  invalidateToken: () => {},
  useMock: true,
})

export const configSet = createEvent<Partial<Config>>()
$config.on(configSet, (config, patch) => ({ ...config, ...patch }))

/* ── события ──────────────────────────────────────────────── */

export const chatsOpened = createEvent()
export const chatSelected = createEvent<string>()
export const chatCreated = createEvent()
export const chatDeleted = createEvent<string>()
export const messageSent = createEvent<string>()
export const draftChanged = createEvent<string>()

/** Файлы, выбранные перед отправкой. Живут до отправки вопроса. */
export const attachmentsAdded = createEvent<globalThis.File[]>()
export const attachmentsCleared = createEvent()

export const $attachments = createStore<File[]>([])
  .on(attachmentsAdded, (current, files) => [...current, ...files])
  .reset(attachmentsCleared, messageSent)

/** Текст в поле ввода. Живёт в сторе, чтобы пережить перерисовку Composer. */
export const $draft = createStore("")
  .on(draftChanged, (_, text) => text)
  // После отправки поле очищается — иначе отправленный текст остаётся в нём.
  .reset(messageSent)

/* ── эффекты ──────────────────────────────────────────────── */

const loadChatsFx = createEffect(() => listChats())
const loadMessagesFx = createEffect((chatId: string) => getMessages(chatId))
const createChatFx = createEffect(() => createChat())
const deleteChatFx = createEffect((chatId: string) => deleteChat(chatId))

const appendMessageFx = createEffect(
  ({ chatId, message }: { chatId: string; message: ChatMessage }) =>
    appendMessage(chatId, message),
)

/**
 * Один запрос `/answers` без истории диалога — диалоговая память вне MVP
 * (ТЗ 1.2). На 401 токен обновляется и запрос повторяется ровно один раз;
 * второй 401 доходит до пользователя как требование войти заново (ТЗ 8.4).
 */
const answerFx = createEffect(async (question: string): Promise<AnswerResponse> => {
  const { apiUrl, kbId, token, invalidateToken, useMock } = $config.getState()
  if (useMock) return mockAnswer(question)

  try {
    return await generateAnswer({ kb_id: kbId, question }, { apiUrl, token: await token() })
  } catch (error) {
    if (!(error instanceof ApiRequestError) || error.status !== 401) throw error
    invalidateToken()
    return generateAnswer({ kb_id: kbId, question }, { apiUrl, token: await token() })
  }
})

/** Всё, что делает один шаг диалога: реплика пользователя. */
type Turn = {
  userMessage: ChatMessage
}

/** Возврат шага. Тип задан явно: ниже `$chatStore` выводится из него. */
type TurnResult = {
  chatId: string
  requestId: string
  answer: ChatMessage
}

const sendTurnFx = createEffect(async ({ userMessage }: Turn): Promise<TurnResult> => {
  // Диалога ещё нет — первый вопрос создаёт его на лету.
  const chatId = $chatStore.getState().activeChatId ?? (await createChatFx()).id

  await appendMessageFx({ chatId, message: userMessage })

  const response = await answerFx(userMessage.text)
  const answer: ChatMessage = {
    id: crypto.randomUUID(),
    role: "assistant",
    text: response.answer,
    citations: response.citations,
    createdAt: new Date().toISOString(),
  }

  await appendMessageFx({ chatId, message: answer })
  return { chatId, requestId: response.request_id, answer }
})

/**
 * Параметры шага собираются один раз: id реплики попадает и в оптимистичное
 * сообщение в UI, и в запись в IndexedDB — иначе это были бы две разные реплики.
 */
const turnRequested = sample({
  clock: messageSent,
  filter: (text) => text.trim().length > 0,
  fn: (text): Turn => ({
    userMessage: {
      id: crypto.randomUUID(),
      role: "user",
      text: text.trim(),
      createdAt: new Date().toISOString(),
    },
  }),
})

/* ── стор ─────────────────────────────────────────────────── */

type State = {
  chats: Chat[]
  activeChatId: string | null
  messages: ChatMessage[]
  status: ChatStatus
  /** Текст ошибки для пользователя: технические коды в UI не попадают. */
  error: string | null
  /** request_id сервера — нужен при разборе инцидентов. */
  requestId: string | null
  loadingChats: boolean
  loadingMessages: boolean
}

const initial: State = {
  chats: [],
  activeChatId: null,
  messages: [],
  status: "idle",
  error: null,
  requestId: null,
  loadingChats: false,
  loadingMessages: false,
}

// `.on(effect)` срабатывает на вызове эффекта — так включается флаг загрузки.
export const $chatStore = createStore<State>(initial)
  .on(loadChatsFx, (state) => ({ ...state, loadingChats: true }))
  .on(loadChatsFx.doneData, (state, chats) => ({ ...state, chats, loadingChats: false }))
  .on(loadChatsFx.fail, (state, { error }) => ({
    ...state,
    loadingChats: false,
    status: "error",
    error: describeError(error),
  }))

  .on(loadMessagesFx, (state) => ({ ...state, loadingMessages: true }))
  .on(loadMessagesFx.done, (state, { params, result }) =>
    // Выдача на устаревший chatId игнорируется: пользователь уже переключился.
    params === state.activeChatId
      ? { ...state, messages: result, loadingMessages: false }
      : state,
  )
  .on(loadMessagesFx.fail, (state) => ({ ...state, loadingMessages: false }))

  .on(chatSelected, (state, activeChatId) => ({
    ...state,
    activeChatId,
    messages: [],
    status: "idle",
    error: null,
  }))
  // Транскрипт чистим сразу, а не по done: под новым диалогом его ещё нет.
  .on(chatCreated, (state) => ({ ...state, messages: [], status: "idle", error: null }))
  .on(createChatFx.doneData, (state, chat) => ({
    ...state,
    chats: [chat, ...state.chats],
    activeChatId: chat.id,
  }))

  // Активный диалог закрываем сразу, не дожидаясь IndexedDB: удалённый
  // транскрипт не должен мелькать на экране. Список перечитается по done.
  .on(chatDeleted, (state, chatId) =>
    chatId === state.activeChatId
      ? { ...state, activeChatId: null, messages: [], status: "idle" }
      : state,
  )

  .on(sendTurnFx.doneData, (state, { chatId, requestId, answer }) => ({
    ...state,
    activeChatId: chatId,
    // Превью в списке диалогов — последнее сообщение; порядок задаёт createdAt.
    chats: state.chats.map((chat) =>
      chat.id === chatId ? { ...chat, preview: answer.text } : chat,
    ),
    messages: [...state.messages, answer],
    status: "idle",
    requestId,
  }))
  .on(sendTurnFx.fail, (state, { error }) => ({
    ...state,
    status: "error",
    error: describeError(error),
    requestId: error instanceof ApiRequestError ? (error.requestId ?? null) : null,
  }))

  // Реплика пользователя появляется сразу — не дожидаясь ни IndexedDB, ни сети.
  .on(turnRequested, (state, { userMessage }) => ({
    ...state,
    messages: [...state.messages, userMessage],
    status: "loading",
    error: null,
  }))

/* ── селекторы ────────────────────────────────────────────── */

export const $messages = $chatStore.map((state) => state.messages)
export const $chats = $chatStore.map((state) => state.chats)
export const $activeChatId = $chatStore.map((state) => state.activeChatId)
export const $status = $chatStore.map((state) => state.status)
export const $error = $chatStore.map((state) => state.error)
export const $requestId = $chatStore.map((state) => state.requestId)
export const $isLoading = $chatStore.map(
  (state) => state.loadingChats || state.loadingMessages,
)
export const $isBusy = $chatStore.map((state) => state.status === "loading")
/** Отправить можно непустую реплику и только вне активного запроса. */
export const $canSend = combine(
  $draft,
  $isBusy,
  (draft, busy) => !busy && draft.trim().length > 0,
)

/* ── цепочки ──────────────────────────────────────────────── */

sample({ clock: turnRequested, target: sendTurnFx })
sample({ clock: chatsOpened, target: loadChatsFx })
sample({ clock: chatSelected, target: loadMessagesFx })
sample({ clock: chatCreated, target: createChatFx })
sample({ clock: chatDeleted, target: deleteChatFx })
sample({ clock: deleteChatFx.done, target: loadChatsFx })

/* ── ошибки ───────────────────────────────────────────────── */

/** Текст ошибки для пользователя: без кодов, статусов и traceback. */
export function describeError(error: unknown): string {
  if (!(error instanceof ApiRequestError)) return "Не удалось выполнить действие. Попробуйте ещё раз."

  switch (error.code) {
    case "LLM_UNAVAILABLE":
      return "Генерация ответа временно недоступна. Попробуйте ещё раз."
    case "CONFIGURATION_MISSING":
    case "CONFIGURATION_INVALID":
      return "База знаний не настроена. Обратитесь к администратору."
    case "UNAUTHORIZED":
    case "FORBIDDEN":
    case "FORBIDDEN_ERROR":
      return "Недостаточно прав для этого запроса."
    case "RATE_LIMITED":
      return "Слишком много запросов. Подождите немного и повторите."
    default:
      return error.status === 0
        ? "Сервис недоступен. Проверьте соединение."
        : "Не удалось получить ответ. Попробуйте ещё раз."
  }
}