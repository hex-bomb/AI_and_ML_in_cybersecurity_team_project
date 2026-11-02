import type { Citation } from "@/shared/types/api"

export type ChatRole = "user" | "assistant"

export type AttachmentState = "idle" | "uploading" | "processing" | "done"

export type Attachment = {
  id: string
  name: string
  /** Size in bytes, formatted for display. */
  size: string
  state: AttachmentState
  /** 0-100 while uploading/processing. */
  progress?: number
  media?: "icon" | "image"
}

/**
 * Источники ответа приходят с сервера в snake_case и кладутся в IndexedDB
 * без преобразования — конвертации на границе хранилища нет.
 */
export type { Citation }

export type ChatMessage = {
  id: string
  role: ChatRole
  /** Plain text. Assistant text grows while streaming. */
  text: string
  createdAt: string
  attachments?: Attachment[]
  /** Только для ответов ассистента. */
  citations?: Citation[]
}

export type Chat = {
  id: string
  title: string
  /** Транскрипт лежит в таблице `messages`, а не внутри чата. */
  messages: ChatMessage[]
  /** Последнее сообщение — подпись в списке диалогов. */
  preview: string
  /** Меняется при каждом добавлении сообщения; в отличие от `createdAt`. */
  updatedAt: string
  createdAt?: string
  pinned?: boolean
  unread?: boolean
}

/**
 * Сообщение в том виде, в каком оно лежит в IndexedDB: чат — это колонка,
 * а не уровень вложенности, поэтому строка адресуема сама по себе.
 */
export type Message = ChatMessage & { chatId: string }

/** Сообщение на входе в хранилище — id и время проставляются там же. */
export type NewMessage = Omit<ChatMessage, "id" | "createdAt"> & {
  id?: string
  createdAt?: string
}

/** Статус одного шага диалога: от отправки реплики до готового ответа. */
export type ChatStatus = "idle" | "loading" | "error"