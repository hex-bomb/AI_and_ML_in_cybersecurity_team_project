export type TokenProvider = {
  get: () => Promise<string>
  invalidate: () => void
}

/**
 * Токен живёт только в памяти вкладки — в localStorage его не кладём (ТЗ 8.5).
 *
 * get() кэширует промис, поэтому параллельные запросы не вызывают getToken дважды.
 * invalidate() сбрасывает кэш: вызывающий код делает это один раз на 401,
 * перезапрашивает токен и повторяет запрос вручную.
 */
export function createTokenProvider(getToken: () => Promise<string>): TokenProvider {
  let cached: Promise<string> | null = null

  return {
    get() {
      if (!cached) {
        cached = getToken().catch((error: unknown) => {
          cached = null // неудачу не кэшируем — следующий вызов попробует снова
          throw error
        })
      }
      return cached
    },
    invalidate() {
      cached = null
    },
  }
}