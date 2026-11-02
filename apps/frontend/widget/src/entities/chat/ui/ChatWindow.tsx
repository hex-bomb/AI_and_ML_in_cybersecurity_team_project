import { useEffect, useState } from "react"
import { useUnit } from "effector-react"
import { AnimatePresence, motion } from "motion/react"
import { LockIcon, PanelLeftIcon } from "lucide-react"

import { Button } from "@/components/ui/button"
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert"
import {
  MessageScroller,
  MessageScrollerButton,
  MessageScrollerContent,
  MessageScrollerItem,
  MessageScrollerProvider,
  MessageScrollerViewport,
} from "@/components/ui/message-scroller"
import { Skeleton } from "@/components/ui/skeleton"
import { ChatComposer } from "@/entities/chat/ui/ChatComposer"
import { ChatList } from "@/entities/chat/ui/ChatList"
import { MessageRow, ThinkingBubble } from "@/entities/chat/ui/MessageRow"
import {
  $chatEnabled,
  $config,
  $error,
  $isBusy,
  $isLoading,
  $messages,
  $requestId,
  $sessionRole,
  chatsOpened,
} from "@/entities/chat/model/chatStore"

/**
 * Точка входа панели: пока хост не прислал конфиг — ожидание, без сессии —
 * заглушка. Вход живёт на странице-хосте, виджет лишь получает сессию.
 */
export function ChatWindow() {
  const [config, enabled] = useUnit([$config, $chatEnabled])

  if (!config.useMock && !config.kbId) {
    return (
      <div className="flex h-full items-center justify-center p-6 text-sm text-muted-foreground">
        Загрузка…
      </div>
    )
  }

  if (!enabled) return <LockedChat />

  return <ChatView />
}

/** Без сессии чат выключен целиком: ни списка диалогов, ни поля ввода. */
function LockedChat() {
  return (
    <div className="flex h-full flex-col">
      <header className="aks-drag flex cursor-grab items-center gap-2 border-b px-2 py-2 active:cursor-grabbing">
        <h2 className="truncate text-sm font-medium">Чат-бот</h2>
      </header>
      <div className="flex flex-1 flex-col items-center justify-center gap-2 px-6 text-center">
        <LockIcon className="size-5 text-muted-foreground" />
        <p className="text-sm font-medium">Чат доступен после входа</p>
        <p className="text-xs text-muted-foreground">
          Нажмите «Войти» в шапке сайта: клиент и сотрудник видят разные документы
          базы знаний.
        </p>
      </div>
    </div>
  )
}

/** Окно виджета: список диалогов, транскрипт, поле ввода. */
function ChatView() {
  const { messages, loading, error, requestId, busy, role } = useUnit({
    messages: $messages,
    loading: $isLoading,
    error: $error,
    requestId: $requestId,
    busy: $isBusy,
    role: $sessionRole,
  })
  const [openChats] = useUnit([chatsOpened])
  const [listOpen, setListOpen] = useState(true)

  // Роль меняет базу переписки, поэтому вход и выход перечитывают список.
  useEffect(() => {
    openChats()
  }, [openChats, role])

  return (
    <div className="flex h-full min-h-0">
      <AnimatePresence initial={false}>
        {listOpen ? (
          <motion.div
            key="list"
            initial={{ width: 0, opacity: 0 }}
            animate={{ width: 230, opacity: 1 }}
            exit={{ width: 0, opacity: 0 }}
            transition={{ type: "spring", stiffness: 380, damping: 36 }}
            className="min-w-0 shrink-0 overflow-hidden"
          >
            <ChatList />
          </motion.div>
        ) : null}
      </AnimatePresence>

      <div className="flex min-w-0 flex-1 flex-col">
        {/* aks-drag — ручка перетаскивания: её ищет Rnd по dragHandleClassName.
            Без класса на самом header панель не таскалась. */}
        <header className="aks-drag flex cursor-grab items-center gap-2 border-b px-2 py-2 active:cursor-grabbing">
          <Button
            size="icon-sm"
            variant="ghost"
            aria-label="Список диалогов"
            aria-expanded={listOpen}
            className="transition-transform active:scale-90"
            onClick={() => setListOpen((v) => !v)}
          >
            <PanelLeftIcon />
          </Button>
          <h2 className="truncate text-sm font-medium">Чат-бот</h2>
        </header>

        {error ? (
          <Alert variant="destructive" className="m-3 rounded-xl">
            <AlertTitle>Ошибка</AlertTitle>
            <AlertDescription>
              {error}
              {requestId ? <span className="ml-1 opacity-70">ID: {requestId}</span> : null}
            </AlertDescription>
          </Alert>
        ) : null}

        {loading ? (
          <div className="flex flex-col gap-4 p-3">
            <Skeleton className="h-12 w-2/3 self-end rounded-xl" />
            <Skeleton className="h-4 w-full" />
            <Skeleton className="h-4 w-4/5" />
            <Skeleton className="h-12 w-1/2 self-end rounded-xl" />
          </div>
        ) : (
          <MessageScrollerProvider autoScroll defaultScrollPosition="end">
            <MessageScroller>
              <MessageScrollerViewport aria-label="Сообщения">
                <MessageScrollerContent className="gap-4 p-3">
                  {messages.length === 0 ? (
                    <p className="py-6 text-center text-sm text-muted-foreground">
                      Здесь будет список сообщений
                    </p>
                  ) : (
                    messages.map((message) => (
                      <MessageScrollerItem
                        key={message.id}
                        messageId={message.id}
                        scrollAnchor={message.role === "user"}
                      >
                        <MessageRow message={message} />
                      </MessageScrollerItem>
                    ))
                  )}

                  {/* Пока модель отвечает, показываем индикатор генерации. */}
                  {busy ? (
                    <MessageScrollerItem messageId="thinking" scrollAnchor>
                      <ThinkingBubble />
                    </MessageScrollerItem>
                  ) : null}
                </MessageScrollerContent>
              </MessageScrollerViewport>
              <MessageScrollerButton className="right-3 bottom-3 left-auto translate-x-0" />
            </MessageScroller>
          </MessageScrollerProvider>
        )}

        <ChatComposer />
      </div>
    </div>
  )
}
