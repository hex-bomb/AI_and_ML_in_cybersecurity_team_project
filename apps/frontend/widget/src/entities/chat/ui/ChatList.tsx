import { useUnit } from "effector-react"
import { motion } from "motion/react"
import { CheckIcon, PlusIcon, TrashIcon, XIcon } from "lucide-react"

import { Button } from "@/components/ui/button"
import { Input } from "@/components/ui/input"
import { Skeleton } from "@/components/ui/skeleton"
import {
  $activeChatId,
  $chats,
  $isLoading,
  chatCreated,
  chatDeleted,
  chatSelected,
} from "@/entities/chat/model/chatStore"
import { useRef, useState } from "react"

/** Список диалогов из IndexedDB. Только shadcn. */
export function ChatList() {
  const [query, setQuery] = useState("")
  // Индекс диалога в фокусе: null — фокус в поле поиска.
  const [activeIndex, setActiveIndex] = useState<number | null>(null)
  const searchRef = useRef<HTMLInputElement>(null)
  const itemsRef = useRef<(HTMLButtonElement | null)[]>([])

  const { chats, activeId, loading } = useUnit({
    chats: $chats,
    activeId: $activeChatId,
    loading: $isLoading,
  })
  const [open, create, remove] = useUnit([chatSelected, chatCreated, chatDeleted])
  // Диалог, ожидающий подтверждения удаления: null — ничего не удаляем.
  const [pendingDelete, setPendingDelete] = useState<string | null>(null)

  const filtered = query.trim()
    ? chats.filter((chat) =>
      `${chat.title} ${chat.preview ?? ""}`.toLowerCase().includes(query.trim().toLowerCase())
    )
    : chats

  return (
    <aside className="flex h-full w-[230px] flex-col gap-2 border-r bg-muted/40 p-2">
      <div className="flex gap-1">
        <Input
          ref={searchRef}
          value={query}
          onChange={(e) => setQuery(e.target.value)}
          // Стрелки переводят фокус между полем поиска и списком диалогов.
          onKeyDown={(e) => {
            if (e.key === "ArrowDown" || e.key === "ArrowUp") {
              if (filtered.length === 0) return
              e.preventDefault()
              const step = e.key === "ArrowDown" ? 1 : -1
              const next = activeIndex === null ? (step > 0 ? 0 : filtered.length - 1) : activeIndex + step
              setActiveIndex(next)
              itemsRef.current[next]?.focus()
            }

            if (e.key === "Escape") {
              setActiveIndex(null)
              searchRef.current?.focus()
            }
          }}
          label="Поиск"
          placeholder="Поиск"
          className="min-w-0 flex-1 [&_input]:h-9 [&_input]:pt-2.5 [&_input]:pb-1"
        />
        <Button
          variant="outline"
          aria-label="Новый диалог"
          className="size-9 shrink-0 rounded-xl"
          onClick={() => create()}
        >
          <PlusIcon />
        </Button>
      </div>

      <div className="scroll-fade min-h-0 flex-1 overflow-y-auto">
        {loading ? (
          <ul className="flex flex-col gap-2" aria-hidden>
            {[0, 1, 2, 3].map((i) => (
              <li key={i}>
                <Skeleton className="h-9 w-full rounded-xl" />
              </li>
            ))}
          </ul>
        ) : filtered.length === 0 ? (
          <p className="px-2 py-6 text-center text-sm text-muted-foreground">
            {query.trim() ? "Ничего не найдено" : "Пока нет диалогов"}
          </p>
        ) : (
          <ul className="flex flex-col gap-2">
            {filtered.map((chat, index) => (
              <li key={chat.id} className="group relative">
                <motion.button
                  ref={(el) => {
                    itemsRef.current[index] = el
                  }}
                  whileHover={{ x: 2 }}
                  whileTap={{ scale: 0.98 }}
                  transition={{ type: "spring", stiffness: 500, damping: 30 }}
                  // Стрелка вверх с первого элемента возвращает фокус в поиск.
                  onKeyDown={(e) => {
                    if (e.key === "ArrowUp" && index === 0) {
                      e.preventDefault()
                      setActiveIndex(null)
                      searchRef.current?.focus()
                    }

                    if (e.key === "ArrowDown") {
                      e.preventDefault()
                      const next = index + 1
                      if (next < filtered.length) {
                        setActiveIndex(next)
                        itemsRef.current[next]?.focus()
                      }
                    }

                    if (e.key === "Escape") {
                      setActiveIndex(null)
                      searchRef.current?.focus()
                    }
                  }}
                  onClick={() => {
                    setActiveIndex(index)
                    open(chat.id)
                  }}
                  className={`w-full rounded-xl border px-3 py-2 pr-8 text-left text-sm transition-colors ${chat.id === activeId
                      ? "border-[#0071e3] bg-[#0071e3]/10 text-[#0071e3]"
                      : "border-transparent bg-accent hover:bg-accent/80"
                    }`}
                >
                  <span className="line-clamp-2">{chat.preview || chat.title}</span>
                </motion.button>

                {/* Удаление: появляется при наведении или фокусе, требует подтверждения. */}
                {pendingDelete === chat.id ? (
                  <span className="absolute top-1/2 right-1.5 flex -translate-y-1/2 items-center gap-0.5">
                    <button
                      type="button"
                      aria-label={`Удалить диалог «${chat.preview || chat.title}»`}
                      onClick={() => {
                        setPendingDelete(null)
                        remove(chat.id)
                      }}
                      className="flex size-6 items-center justify-center rounded-md bg-destructive text-destructive-foreground transition-transform hover:scale-105"
                    >
                      <CheckIcon className="size-3.5" />
                    </button>
                    <button
                      type="button"
                      aria-label="Отмена"
                      onClick={() => setPendingDelete(null)}
                      className={`flex size-6 items-center justify-center rounded-md transition-colors hover:bg-[#0071e3]/15 ${chat.id === activeId ? "text-[#0071e3]" : "text-muted-foreground"
                        }`}
                    >
                      <XIcon className="size-3.5" />
                    </button>
                  </span>
                ) : (
                  <button
                    type="button"
                    aria-label={`Удалить диалог «${chat.preview || chat.title}»`}
                    onClick={() => setPendingDelete(chat.id)}
                    className={`absolute top-1/2 right-1.5 flex size-6 -translate-y-1/2 items-center justify-center rounded-md opacity-0 transition-all group-hover:opacity-100 focus-visible:opacity-100 ${chat.id === activeId
                        ? "text-[#0071e3] hover:bg-destructive/10 hover:text-destructive"
                        : "text-muted-foreground hover:bg-destructive/10 hover:text-destructive"
                      }`}
                  >
                    <TrashIcon className="size-3.5" />
                  </button>
                )}
              </li>
            ))}
          </ul>
        )}
      </div>
    </aside>
  )
}