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
 * Local persistence. Chats and messages never leave the browser — the server
 * only ever receives generation requests. Both tables live in one IndexedDB
 * database so a message write and the chat's `updatedAt` bump stay atomic.
 */

const DB_NAME = "chat-widget"

/** Row shape of `chats`: metadata only, the transcript is a separate table. */
type ChatRow = {
  id: string
  title: string
  createdAt: string
  /** Last write time. Used to reorder the sidebar; `createdAt` never changes. */
  updatedAt: string
  /** Text of the last message, kept denormalised for the sidebar. */
  preview: string
  pinned?: boolean
  unread?: boolean
}

/** Row shape of `messages`. */
type MessageRow = {
  id: string
  chatId: string
  role: ChatRole
  text: string
  citations: Citation[]
  createdAt: string
  attachments?: Attachment[]
}

export const db = new Dexie(DB_NAME) as Dexie & {
  chats: EntityTable<ChatRow, "id">
  messages: EntityTable<MessageRow, "id">
}

// `&id` is the primary key. Compound `[chatId+createdAt]` serves the transcript
// read, so it walks the index in order instead of filtering the whole table.
db.version(1).stores({
  chats: "&id, title, createdAt, updatedAt",
  messages: "&id, chatId, createdAt, [chatId+createdAt]",
})

function nowIso() {
  return new Date().toISOString()
}

/** `crypto.randomUUID` needs a secure context; fall back for plain http embeds. */
function newId(): string {
  const cryptoRef = globalThis.crypto
  if (cryptoRef?.randomUUID) return cryptoRef.randomUUID()
  return `${Date.now().toString(36)}-${Math.random().toString(36).slice(2, 10)}`
}

/** Chat rows have no transcript of their own; callers read it with getMessages. */
function toChat(row: ChatRow): Chat {
  return { ...row, messages: [] }
}

/** Sidebar order: newest first. */
export async function listChats(): Promise<Chat[]> {
  const rows = await db.chats.orderBy("createdAt").reverse().toArray()
  return rows.map(toChat)
}

/** Transcript order: oldest first. */
export async function getMessages(chatId: string): Promise<Message[]> {
  return db.messages
    .where("[chatId+createdAt]")
    .between([chatId, ""], [chatId, "￿"])
    .toArray()
}

/** Creates an empty conversation, optionally pre-titled. */
export async function createChat(title = "Новый диалог"): Promise<Chat> {
  const createdAt = nowIso()
  const row: ChatRow = {
    id: newId(),
    title,
    createdAt,
    updatedAt: createdAt,
    preview: "",
  }
  await db.chats.add(row)
  return toChat(row)
}

/** Removes the conversation and its messages together. */
export async function deleteChat(chatId: string): Promise<void> {
  await db.transaction("rw", db.chats, db.messages, async () => {
    await db.messages.where("chatId").equals(chatId).delete()
    await db.chats.delete(chatId)
  })
}

/**
 * Appends a message and refreshes the chat's sidebar fields in one transaction,
 * so the list order and the transcript can never disagree.
 *
 * `id` and `createdAt` are filled in when the caller leaves them out.
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

  await db.transaction("rw", db.chats, db.messages, async () => {
    await db.messages.add(row)

    // Дописанное сообщение может оказаться старше уже сохранённого (например,
    // при восстановлении истории). `updatedAt` откатываться не должен — иначе
    // чат уедет вниз списка, поэтому решает более свежий из двух.
    const chat = await db.chats.get(chatId)
    if (!chat) return

    const patch: Partial<ChatRow> = {}
    if (createdAt >= chat.updatedAt) {
      patch.updatedAt = createdAt
      patch.preview = message.text
    }

    if (Object.keys(patch).length > 0) await db.chats.update(chatId, patch)
  })

  return row
}