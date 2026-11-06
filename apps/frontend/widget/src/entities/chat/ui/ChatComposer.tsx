import { useRef } from "react"
import { useUnit } from "effector-react"
import { motion } from "motion/react"
import { ArrowUpIcon, PaperclipIcon, XIcon } from "lucide-react"

import { Button } from "@/components/ui/button"
import { Textarea } from "@/components/ui/textarea"
import {
  $attachments,
  $canSend,
  $draft,
  $isBusy,
  $pageContext,
  attachmentsAdded,
  draftChanged,
  messageSent,
  pageContextChanged,
} from "@/entities/chat/model/chatStore"

/** Поле ввода. Отправка — Enter или кнопка, слева — добавление вложений. */
export function ChatComposer() {
  const { draft, busy, canSend, files, context, setDraft, send, addFiles, clearContext } = useUnit({
    draft: $draft,
    busy: $isBusy,
    canSend: $canSend,
    files: $attachments,
    context: $pageContext,
    setDraft: draftChanged,
    send: messageSent,
    addFiles: attachmentsAdded,
    clearContext: pageContextChanged,
  })

  // Нативный input скрыт: кнопка отправляет в него файл через клик.
  const pickerRef = useRef<HTMLInputElement>(null)

  // Что именно уйдёт на сервер: выделение информативнее заголовка страницы.
  const contextLabel = context?.selection?.text ?? context?.title ?? context?.url ?? "Страница"

  return (
    <div className="border-t">
      {/* Контекст виден до отправки: пользователь может его убрать, а не догадываться. */}
      {context && (
        <div className="flex items-center gap-1.5 px-2 pt-1.5 text-xs text-muted-foreground">
          <span className="shrink-0 rounded-md bg-muted px-1.5 py-0.5">Контекст</span>
          <span className="min-w-0 flex-1 truncate" title={contextLabel}>
            {contextLabel}
          </span>
          <Button
            aria-label="Убрать контекст страницы"
            variant="ghost"
            size="icon-xs"
            onClick={() => clearContext(null)}
          >
            <XIcon />
          </Button>
        </div>
      )}

      <div className="flex items-center gap-1.5 p-2">
      <Textarea
        value={draft}
        disabled={busy}
        aria-label="Сообщение"
        placeholder={files.length ? "Спрашивайте с учётом вложений" : "Сообщение"}
        onChange={(e) => setDraft(e.target.value)}
        onKeyDown={(e) => {
          if (e.key === "Enter" && !e.shiftKey) {
            e.preventDefault()
            if (canSend) send(draft)
          }
        }}
        className="h-9 max-h-32 min-h-9 flex-1 resize-none rounded-xl [scrollbar-width:none] [&::-webkit-scrollbar]:hidden"
      />

      <motion.div whileTap={{ scale: 0.88 }} className="shrink-0">
        <Button
          aria-label="Добавить вложение"
          variant="outline"
          className="size-9 rounded-xl"
          disabled={busy}
          onClick={() => pickerRef.current?.click()}
        >
          <PaperclipIcon />
        </Button>
      </motion.div>

      <motion.div whileTap={{ scale: 0.88 }} className="shrink-0">
        <Button
          aria-label="Отправить"
          className="size-9 rounded-xl"
          disabled={!canSend}
          onClick={() => send(draft)}
        >
          <ArrowUpIcon />
        </Button>
      </motion.div>

      <input
        ref={pickerRef}
        type="file"
        multiple
        className="hidden"
        accept=".md,.txt,.pdf,.docx,.csv,.json"
        onChange={(event) => {
          const picked = Array.from(event.target.files ?? [])
          if (picked.length) addFiles(picked)
          // Сбрасываем value, иначе повторный выбор того же файла не сработает.
          event.target.value = ""
        }}
      />
      </div>
    </div>
  )
}
