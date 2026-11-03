import { useEffect, useRef } from "react"

import { type Config, configSet, refreshSession } from "@/entities/chat/model/chatStore"

type HostCommand = "open" | "close" | "toggle"

type HostPayload = {
  apiUrl?: string
  kbId?: string
  useMock?: boolean
}

/**
 * Origin страницы-хоста. Сначала берём из referrer: он указывает на страницу,
 * которая вставила iframe. Если хост вырезал referrer политикой, остаётся "*"
 * до первого входящего сообщения — по нему origin и запомним.
 */
let parentOrigin = originOf(document.referrer) ?? "*"

function originOf(url: string): string | null {
  if (!url) return null
  try {
    return new URL(url).origin
  } catch {
    return null
  }
}

/** Отправить сообщение хосту, не раскрывая его origin посторонним документам. */
export function postToHost(message: Record<string, unknown>): void {
  window.parent.postMessage(message, parentOrigin)
}

/** Команды от страницы-хоста через aks-widget.js. */
export function useHostBridge(onCommand: (type: HostCommand) => void) {
  // Ref, иначе инлайновая стрелка переподписывала бы слушатель на каждый рендер.
  const handler = useRef(onCommand)

  useEffect(() => {
    handler.current = onCommand
  }, [onCommand])

  useEffect(() => {
    function onMessage(event: MessageEvent) {
      const data = event.data
      // Управляет виджетом только хост и только документированным сообщением:
      // соседний фрейм на той же странице до window.parent не достанет.
      if (!data || typeof data !== "object" || data.source !== "aks-host") return
      if (window.parent !== window && event.source !== window.parent) return

      // Origin родителя — единственный надёжный признак отправителя.
      // "null" бывает у sandbox-фреймов и file://, ему верить нельзя.
      if (event.origin && event.origin !== "null") parentOrigin = event.origin

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
    if (window.parent !== window) postToHost({ source: "aks-widget", type: "ready" })

    return () => window.removeEventListener("message", onMessage)
  }, [])
}
