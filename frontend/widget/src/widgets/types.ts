import type { Citation } from "@/shared/types/api"

/** Опции виджета — контракт ТЗ 8.5. */
export interface WidgetOptions {
  container: HTMLElement
  api_url: string
  kb_id: string
  get_token: () => Promise<string>
  theme?: "light" | "dark"
  on_open_source?: (citation: Citation) => void
}

export interface WidgetHandle {
  destroy(): void
}

export type { Citation }