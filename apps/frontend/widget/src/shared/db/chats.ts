import Dexie, { type EntityTable } from "dexie"

import type {
  Attachment,
  Chat,
  ChatRole,
  Citation,
  Message,
  NewMessage,
} from "@/shared/types/chat"

/**
 * Локальное хранилище. Диалоги и сообщения не уходят на сервер — он получает
 * только запросы на генерацию. Обе таблицы живут в одной базе IndexedDB,
 * поэтому запись сообщения и обновление `updatedAt` диалога атомарны.
 */

/** Строка таблицы `chats`: только метаданные, транскрипт лежит отдельно. */
type ChatRow = {
  id: string
  title: string
  createdAt: string
  /** Время последней записи: по нему сортируется список; `createdAt` не меняется. */
  updatedAt: string
  /** Текст последнего сообщения — денормализация ради списка диалогов. */
  preview: string
  pinned?: boolean
  unread?: boolean
}

/** Строка таблицы `messages`. */
type MessageRow = {
  id: string
  chatId: string
  role: ChatRole
  text: string
  citations: Citation[]
  createdAt: string
  attachments?: Attachment[]
}

type Store = Dexie & {
  chats: EntityTable<ChatRow, "id">
  messages: EntityTable<MessageRow, "id">
}

// Одна база на роль: клиент не должен видеть в списке диалоги, начатые под
// сотрудником (и наоборот), — они лежат на одном устройстве. `guest` — режим
// моков, когда входа нет вовсе.
let scope = "guest"
let current: Store | null = null

/** Вызывается после успешного входа; смена роли переключает базу. */
export function setStoreRole(role: string) {
  if (role === scope && current) return
  current?.close()
  current = null
  scope = role
}

function db(): Store {
  if (current) return current
  const instance = new Dexie(`chat-widget-${scope}`) as Store
  // `&id` — первичный ключ. Новый индекс требует новой версии схемы Dexie.
  instance.version(1).stores({
    chats: "&id, title, createdAt, updatedAt",
    messages: "&id, chatId, createdAt, [chatId+createdAt]",
  })
  current = instance
  return instance
}

function nowIso() {
  return new Date().toISOString()
}

/** `crypto.randomUUID` есть только в secure context — для http-встраивания фолбэк. */
function newId(): string {
  const cryptoRef = globalThis.crypto
  if (cryptoRef?.randomUUID) return cryptoRef.randomUUID()
  return `${Date.now().toString(36)}-${Math.random().toString(36).slice(2, 10)}`
}

/** В строке диалога нет транскрипта: его читают через getMessages. */
function toChat(row: ChatRow): Chat {
  return { ...row, messages: [] }
}

/** Порядок в списке: новые сверху. */
export async function listChats(): Promise<Chat[]> {
  const rows = await db().chats.orderBy("createdAt").reverse().toArray()
  return rows.map(toChat)
}

/** Порядок в транскрипте: старые сверху. */
export async function getMessages(chatId: string): Promise<Message[]> {
  return db().messages.where("chatId").equals(chatId).sortBy("createdAt")
}

/** Создаёт пустой диалог; заголовок можно задать сразу. */
export async function createChat(title = "Новый диалог"): Promise<Chat> {
  const createdAt = nowIso()
  const row: ChatRow = {
    id: newId(),
    title,
    createdAt,
    updatedAt: createdAt,
    preview: "",
  }
  await db().chats.add(row)
  return toChat(row)
}

/** Удаляет диалог вместе с сообщениями в одной транзакции. */
export async function deleteChat(chatId: string): Promise<void> {
  const store = db()
  await store.transaction("rw", store.chats, store.messages, async () => {
    await store.messages.where("chatId").equals(chatId).delete()
    await store.chats.delete(chatId)
  })
}

/**
 * Дописывает сообщение и обновляет поля диалога для списка в одной транзакции:
 * порядок в списке и транскрипт не могут разойтись.
 *
 * `id` и `createdAt` заполняются, если вызывающий их не передал.
 */
export async function appendMessage(chatId: string, message: NewMessage): Promise<Message> {
  const createdAt = message.createdAt ?? nowIso()
  const row: MessageRow = {
    id: message.id ?? newId(),
    chatId,
    role: message.role,
    text: message.text,
    citations: message.citations ?? [],
    createdAt,
    attachments: message.attachments,
  }

  const store = db()
  await store.transaction("rw", store.chats, store.messages, async () => {
    await store.messages.add(row)

    // Дописанное сообщение может оказаться старше уже сохранённого (например,
    // при восстановлении истории). `updatedAt` откатываться не должен — иначе
    // чат уедет вниз списка, поэтому решает более свежий из двух.
    const chat = await store.chats.get(chatId)
    if (!chat) return

    const patch: Partial<ChatRow> = {}
    if (createdAt >= chat.updatedAt) {
      patch.updatedAt = createdAt
      patch.preview = message.text
    }

    if (Object.keys(patch).length > 0) await store.chats.update(chatId, patch)
  })

  return row
}
