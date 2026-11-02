import { useEffect, useRef } from "react"

import { type Config, configSet, refreshSession } from "@/entities/chat/model/chatStore"

type HostCommand = "open" | "close" | "toggle"

type HostPayload = {
  apiUrl?: string
  kbId?: string
  useMock?: boolean
}

/** Команды от страницы-хоста через aks-widget.js. */
export function useHostBridge(onCommand: (type: HostCommand) => void) {
  // Ref, so a caller passing an inline arrow does not resubscribe on every render.
  const handler = useRef(onCommand)

  useEffect(() => {
    handler.current = onCommand
  }, [onCommand])

  useEffect(() => {
    function onMessage(event: MessageEvent) {
      const data = event.data
      // Only the embedding page may drive the widget, and only with the
      // documented command shape.
      if (!data || typeof data !== "object" || data.source !== "aks-host") return
      if (window.parent && event.source !== window.parent) return

      // Конфиг приходит отдельно: адрес API, база знаний и режим моков.
      if (data.type === "config") {
        const payload = (data.payload ?? {}) as HostPayload
        // Пустые поля не переносим: иначе затёрли бы рабочие значения.
        const patch: Partial<Config> = { useMock: payload.useMock !== false }
        if (payload.apiUrl) patch.apiUrl = payload.apiUrl
        if (payload.kbId) patch.kbId = payload.kbId
        configSet(patch)
        // Сессия живёт в cookie, а не в сообщении. `configure` после входа или
        // выхода — повод перечитать `/auth/me`.
        void refreshSession()
        return
      }

      if (data.type !== "open" && data.type !== "close" && data.type !== "toggle") return
      handler.current(data.type)
    }

    window.addEventListener("message", onMessage)

    // Cookie могла остаться с прошлого визита: проверяем сессию сразу, не
    // дожидаясь первого сообщения от хоста.
    void refreshSession()

    // Сообщаем хосту, что виджет поднялся: конфиг приходит в ответ.
    // Без этого первый postMessage уходит в пустоту — событие load на
    // стороне страницы срабатывает раньше нашего mount.
    if (window.parent !== window) {
      window.parent.postMessage({ source: "aks-widget", type: "ready" }, "*")
    }

    return () => window.removeEventListener("message", onMessage)
  }, [])
}
