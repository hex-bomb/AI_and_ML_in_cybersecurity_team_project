import { combine, createEffect, createEvent, createStore, sample } from "effector"

import { mockAnswer } from "@/shared/api/dev-mock"
import { fetchSession } from "@/shared/api/auth"
import { ApiRequestError } from "@/shared/api/http"
import { generateAnswer } from "@/shared/api/search"
import {
  appendMessage,
  createChat,
  deleteChat,
  getMessages,
  listChats,
  setStoreRole,
} from "@/shared/db/chats"
import type { AnswerResponse, AnswerTurn, Citation } from "@/shared/types/api"
import type { Chat, ChatMessage, ChatStatus } from "@/shared/types/chat"

/* конфигурация виджета */

export type Config = {
  apiUrl: string
  kbId: string
  useMock: boolean
  onOpenSource?: (citation: Citation) => void
}

/** Ставится снаружи — см. widgets/widgetBridge.ts. */
export const $config = createStore<Config>({
  apiUrl: "/api/v1",
  kbId: "",
  useMock: true,
})

/**
 * Пользователь из cookie-сессии. Роль определяет сервер: виджет спрашивает
 * `/auth/me`, а не получает роль из postMessage.
 */
export type Session = { id: string; name: string; role: "user" | "employee" }

export const configSet = createEvent<Partial<Config>>()
$config.on(configSet, (config, patch) => ({ ...config, ...patch }))

/* события */

export const chatsOpened = createEvent()
export const chatSelected = createEvent<string>()
export const chatCreated = createEvent()
export const chatDeleted = createEvent<string>()
export const messageSent = createEvent<string>()
export const draftChanged = createEvent<string>()

/** Сессия пропала (выход или 401): чужой транскрипт больше не показываем. */
export const sessionCleared = createEvent()

/** Пришёл ответ `/auth/me` или `/auth/login`. `null` — гость. */
export const sessionChanged = createEvent<Session | null>()

export const $session = createStore<Session | null>(null)
  .on(sessionChanged, (_, session) => session)
  .reset(sessionCleared)

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

/* эффекты */

const loadChatsFx = createEffect(() => listChats())
const loadMessagesFx = createEffect((chatId: string) => getMessages(chatId))
const createChatFx = createEffect(() => createChat())
const deleteChatFx = createEffect((chatId: string) => deleteChat(chatId))

const appendMessageFx = createEffect(
  ({ chatId, message }: { chatId: string; message: ChatMessage }) =>
    appendMessage(chatId, message),
)

/**
 * Один запрос `/answers`. История диалога уходит на сервер явно: бэкенд берёт
 * последние восемь реплик и передаёт их модели как контекст. Демо-токен не
 * обновляется, поэтому 401 означает «войдите снова», а не повтор запроса.
 */
const answerFx = createEffect(
  async ({ question, history }: { question: string; history: AnswerTurn[] }): Promise<AnswerResponse> => {
    const { apiUrl, kbId, useMock } = $config.getState()
    if (useMock) return mockAnswer(question)

    if ($session.getState() === null) {
      throw new ApiRequestError({
        status: 401,
        code: "UNAUTHORIZED",
        message: "Войдите, чтобы задать вопрос",
      })
    }

    try {
      return await generateAnswer({ kb_id: kbId, question, history }, { apiUrl })
    } catch (error) {
      if (error instanceof ApiRequestError && error.status === 401) sessionCleared()
      throw error
    }
  },
)

/** Всё, что делает один шаг диалога: реплика пользователя и контекст до неё. */
type Turn = {
  userMessage: ChatMessage
  history: AnswerTurn[]
}

/** Возврат шага. Тип задан явно: ниже `$chatStore` выводится из него. */
type TurnResult = {
  chatId: string
  requestId: string
  answer: ChatMessage
}

const sendTurnFx = createEffect(async ({ userMessage, history }: Turn): Promise<TurnResult> => {
  // Диалога ещё нет — первый вопрос создаёт его на лету.
  const chatId = $chatStore.getState().activeChatId ?? (await createChatFx()).id

  await appendMessageFx({ chatId, message: userMessage })

  const response = await answerFx({ question: userMessage.text, history })
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

/** Последние реплики диалога в той форме, которую ждёт `/answers`. */
function historyOf(messages: ChatMessage[]): AnswerTurn[] {
  return messages
    .filter((message) => message.text.trim().length > 0)
    .slice(-8)
    .map((message) => ({ role: message.role, text: message.text }))
}

/**
 * Параметры шага собираются один раз: id реплики попадает и в оптимистичное
 * сообщение в UI, и в запись в IndexedDB — иначе это были бы две разные реплики.
 *
 * Событие объявлено до стора (на него подписан редьюсер), а наполняется ниже —
 * в секции цепочек, где `$chatStore` уже существует.
 */
const turnRequested = createEvent<Turn>()

/* стор */

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
  // Выход из аккаунта не должен оставлять на экране чужую переписку.
  .reset(sessionCleared)

/* селекторы */

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
/** Без сессии виджет открывается, но не принимает вопросы. */
export const $chatEnabled = combine(
  $config,
  $session,
  (config, session) => config.useMock || session !== null,
)

/** Роль из сессии: по ней выбирается база переписки. */
export const $sessionRole = $session.map((session) => session?.role ?? "guest")

/** Отправить можно непустую реплику, вне активного запроса и при сессии. */
export const $canSend = combine(
  $draft,
  $isBusy,
  $chatEnabled,
  (draft, busy, enabled) => enabled && !busy && draft.trim().length > 0,
)

/* цепочки */

// Транскрипт читается из стора до того, как новая реплика попадёт в него,
// поэтому она не окажется в собственном контексте.
sample({
  clock: messageSent,
  source: $chatStore,
  filter: (_, text) => text.trim().length > 0,
  fn: (state, text): Turn => {
    const trimmed = text.trim()
    return {
      userMessage: {
        id: crypto.randomUUID(),
        role: "user",
        text: trimmed,
        createdAt: new Date().toISOString(),
      },
      history: historyOf(state.messages),
    }
  },
  target: turnRequested,
})

sample({ clock: turnRequested, target: sendTurnFx })
sample({ clock: chatsOpened, target: loadChatsFx })
sample({ clock: chatSelected, target: loadMessagesFx })
sample({ clock: chatCreated, target: createChatFx })
sample({ clock: chatDeleted, target: deleteChatFx })
sample({ clock: deleteChatFx.done, target: loadChatsFx })

// Роль выбирает отдельную базу переписки в IndexedDB: клиент не должен видеть
// диалоги сотрудника на том же устройстве.
sessionChanged.watch((session) => {
  setStoreRole(session?.role ?? "guest")
})

/**
 * Перечитать сессию из cookie. Вызывается при загрузке виджета и когда хост
 * сообщает `configure` — после входа или выхода.
 */
export async function refreshSession(): Promise<void> {
  const { apiUrl, useMock } = $config.getState()
  if (useMock) {
    sessionChanged({ id: "demo", name: "Демо", role: "employee" })
    return
  }
  try {
    sessionChanged(await fetchSession({ apiUrl }))
  } catch {
    sessionChanged(null)
  }
}

/* ошибки */

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
