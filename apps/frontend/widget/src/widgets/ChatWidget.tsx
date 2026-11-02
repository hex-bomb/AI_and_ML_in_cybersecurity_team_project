import { AnimatePresence, motion } from "motion/react"
import { MessageCircleIcon, XIcon } from "lucide-react"
import { useCallback, useEffect, useState } from "react"
import { Rnd } from "react-rnd"

import { Button } from "@/components/ui/button"
import { ChatWindow } from "@/entities/chat/ui/ChatWindow"
import { useHostBridge } from "./widgetBridge"

/** Панель открывается ровно над кнопкой. */
const BUTTON = 60
const GAP = 12
const MARGIN = 24

const MIN = { width: 360, height: 380 }
const MAX = { width: 900, height: 900 }

type Box = { x: number; y: number; w: number; h: number }

/** Кнопка в правом нижнем углу; панель привязана к ней и не заезжает на поля. */
function defaultBox(): Box {
  const w = Math.min(Math.round(window.innerWidth / 2), MAX.width, Math.max(MIN.width, window.innerWidth - MARGIN * 2))
  const h = Math.min(Math.round(window.innerHeight * 0.72), MAX.height, Math.max(MIN.height, window.innerHeight - MARGIN * 2))

  return {
    x: Math.max(MARGIN, window.innerWidth - MARGIN - w),
    y: Math.max(MARGIN, window.innerHeight - MARGIN - BUTTON - GAP - h),
    w,
    h,
  }
}

/** Держит панель целиком в окне после ресайза окна или панели. */
function clamp(box: Box): Box {
  const maxX = Math.max(MARGIN, window.innerWidth - MARGIN - box.w)
  const maxY = Math.max(MARGIN, window.innerHeight - MARGIN - box.h)

  return {
    ...box,
    x: Math.min(Math.max(MARGIN, box.x), maxX),
    y: Math.min(Math.max(MARGIN, box.y), maxY),
  }
}

/**
 * Корневой виджет: круглая кнопка + перетаскиваемая и изменяемая по размеру
 * панель. Вся анимация — на motion/react.
 */
export function ChatWidget() {
  const [open, setOpen] = useState(false)
  const [box, setBox] = useState<Box>(defaultBox)

  useHostBridge(
    useCallback(
      (type) => setOpen((v) => (type === "toggle" ? !v : type === "open")),
      []
    )
  )

  useEffect(() => {
    const onResize = () => setBox((b) => clamp(b))
    window.addEventListener("resize", onResize)
    return () => window.removeEventListener("resize", onResize)
  }, [])

  // Панель лежит в прозрачном iframe во весь экран и иначе перекрывала бы
  // клики по странице. Хост по этому сообщению режет область попадания до
  // кнопки, пока панель закрыта.
  useEffect(() => {
    if (window.parent === window) return
    window.parent.postMessage({ source: "aks-widget", type: "state", open }, "*")
  }, [open])

  // Кнопка всегда в правом нижнем углу, панель привязана к ней.
  const btnLeft = window.innerWidth - MARGIN - BUTTON

  return (
    <>
      <AnimatePresence>
        {open ? (
          <Rnd
            bounds="window"
            position={{ x: box.x, y: box.y }}
            size={{ width: box.w, height: box.h }}
            minWidth={MIN.width}
            minHeight={MIN.height}
            maxWidth={MAX.width}
            maxHeight={MAX.height}
            enableResizing
            dragHandleClassName="aks-drag"
            onDragStop={(_, d) => setBox((b) => clamp({ ...b, x: d.x, y: d.y }))}
            onResizeStop={(_, __, ref, ___, d) =>
              setBox(
                clamp({
                  x: d.x,
                  y: d.y,
                  w: ref.offsetWidth,
                  h: ref.offsetHeight,
                })
              )
            }
          >
            <motion.div
              initial={{ opacity: 0, y: 16, scale: 0.96 }}
              animate={{ opacity: 1, y: 0, scale: 1 }}
              exit={{ opacity: 0, y: 16, scale: 0.96 }}
              transition={{ type: "spring", stiffness: 420, damping: 34 }}
              className="h-full w-full overflow-hidden rounded-xl border border-border bg-background shadow-2xl shadow-black/25"
            >
              <ChatWindow />
            </motion.div>
          </Rnd>
        ) : null}
      </AnimatePresence>

      <motion.div
        className="fixed z-50"
        style={{ left: btnLeft, bottom: MARGIN }}
        initial={{ scale: 0.6, opacity: 0 }}
        animate={{ scale: 1, opacity: 1 }}
        transition={{ type: "spring", stiffness: 400, damping: 26 }}
        whileHover={{ scale: 1.07 }}
        whileTap={{ scale: 0.93 }}
      >
        <Button
          size="icon"
          aria-label={open ? "Закрыть чат" : "Открыть чат"}
          onClick={() => setOpen((v) => !v)}
          className="size-[60px] rounded-full"
        >
          <AnimatePresence mode="wait" initial={false}>
            <motion.span
              key={open ? "close" : "open"}
              initial={{ opacity: 0, rotate: -90, scale: 0.6 }}
              animate={{ opacity: 1, rotate: 0, scale: 1 }}
              exit={{ opacity: 0, rotate: 90, scale: 0.6 }}
              transition={{ duration: 0.18 }}
              className="flex"
            >
              {open ? <XIcon className="size-6" /> : <MessageCircleIcon className="size-6" />}
            </motion.span>
          </AnimatePresence>
        </Button>
      </motion.div>
    </>
  )
}
