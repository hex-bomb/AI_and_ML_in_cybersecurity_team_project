import { useEffect, useState } from "react"
import { AnimatePresence, motion } from "motion/react"
import { CheckIcon, CopyIcon, ThumbsDownIcon, ThumbsUpIcon } from "lucide-react"

import {
  Attachment,
  AttachmentContent,
  AttachmentDescription,
  AttachmentTitle,
} from "@/components/ui/attachment"
import { Bubble, BubbleContent } from "@/components/ui/bubble"
import { Button } from "@/components/ui/button"
import { Message, MessageContent } from "@/components/ui/message"
import { MarkdownText } from "@/shared/ui/MarkdownText"
import { cn } from "cn"
import type { ChatMessage } from "@/shared/types/chat"

/** Одно сообщение. Аватаров нет. У ответа — копировать и лайк/дизлайк. */
export function MessageRow({ message, streaming }: { message: ChatMessage; streaming?: boolean }) {
  if (message.role === "user") {
    return (
      <Message align="end">
        <MessageContent>
          <Bubble variant="muted">
            <BubbleContent>
              {message.attachments?.length ? (
                <div className="flex flex-wrap gap-1.5">
                  {message.attachments.map((a) => (
                    <Attachment key={a.id} size="sm" state={a.state}>
                      <AttachmentContent>
                        <AttachmentTitle>{a.name}</AttachmentTitle>
                        <AttachmentDescription>{a.size}</AttachmentDescription>
                      </AttachmentContent>
                    </Attachment>
                  ))}
                </div>
              ) : null}
              <MarkdownText>{message.text}</MarkdownText>
            </BubbleContent>
          </Bubble>
        </MessageContent>
      </Message>
    )
  }

  return (
    <Message align="start">
      <MessageContent>
        <Bubble variant="ghost">
          <BubbleContent>
            <MarkdownText>{message.text}</MarkdownText>
            {streaming ? <Caret /> : null}
          </BubbleContent>
        </Bubble>
        {streaming ? null : <Feedback text={message.text} />}
      </MessageContent>
    </Message>
  )
}

/** Мигающий курсор в конце стримящегося ответа. */
function Caret() {
  return (
    <span className="ml-0.5 inline-block h-3.5 w-[2px] translate-y-0.5 bg-current align-middle animate-pulse" />
  )
}

/**
 * Индикатор работы над ответом.
 *
 * Стриминга в MVP нет (ТЗ 1.2), поэтому показываем его на всё время запроса.
 * Подпись меняется каждые пару секунд — видно, что виджет занят, и не создаётся
 * впечатления, будто он завис на одной и той же фразе.
 */

/** Этапы работы виджета: поиск по базе, проверка источников, сборка ответа. */
const THINKING_STAGES = ["Готовим ответ", "Ищем данные", "Проверяем станки"] as const

const STAGE_INTERVAL_MS = 2200

export function ThinkingBubble() {
  const [stage, setStage] = useState(0)

  useEffect(() => {
    const timer = window.setInterval(
      () => setStage((current) => (current + 1) % THINKING_STAGES.length),
      STAGE_INTERVAL_MS,
    )
    return () => window.clearInterval(timer)
  }, [])

  return (
    <Message align="start">
      <MessageContent>
        <Bubble variant="ghost">
          <BubbleContent>
            <span className="sr-only" role="status">
              {THINKING_STAGES[stage]}
            </span>
            <AnimatePresence mode="wait" initial={false}>
              <motion.span
                key={stage}
                className="shimmer block py-0.5 text-sm text-muted-foreground"
                initial={{ opacity: 0, y: 4 }}
                animate={{ opacity: 1, y: 0 }}
                exit={{ opacity: 0, y: -4 }}
                transition={{ duration: 0.25, ease: "easeOut" }}
                aria-hidden
              >
                {THINKING_STAGES[stage]}
              </motion.span>
            </AnimatePresence>
          </BubbleContent>
        </Bubble>
      </MessageContent>
    </Message>
  )
}

/** Копировать + лайк/дизлайк. Нажатая закрашивается, повтор — снимает. */
function Feedback({ text }: { text: string }) {
  const [vote, setVote] = useState<"up" | "down" | null>(null)
  const [copied, setCopied] = useState(false)

  async function copy() {
    try {
      await navigator.clipboard.writeText(text)
      setCopied(true)
      setTimeout(() => setCopied(false), 1500)
    } catch {
      // Clipboard blocked (insecure context or denied permission) — leave as is.
    }
  }

  return (
    <div className="-mt-1.5 flex gap-0.5">
      <Button
        size="icon-xs"
        variant="ghost"
        aria-label={copied ? "Скопировано" : "Скопировать"}
        className="active:scale-90"
        onClick={copy}
      >
        {copied ? <CheckIcon /> : <CopyIcon />}
      </Button>

      {([
        ["up", ThumbsUpIcon, "Нравится"],
        ["down", ThumbsDownIcon, "Не нравится"],
      ] as const).map(([key, Icon, label]) => (
        <motion.div key={key} whileTap={{ scale: 0.8 }}>
          <Button
            size="icon-xs"
            variant="ghost"
            aria-label={label}
            aria-pressed={vote === key}
            onClick={() => setVote((v) => (v === key ? null : key))}
            className={cn(
              "transition-colors",
              vote === key && "bg-primary text-primary-foreground hover:bg-primary/80"
            )}
          >
            <Icon />
          </Button>
        </motion.div>
      ))}
    </div>
  )
}
