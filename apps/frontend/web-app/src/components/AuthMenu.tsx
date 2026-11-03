import { useEffect, useState } from "react"
import { AnimatePresence, motion } from "motion/react"

/**
 * Вход в демо-базу знаний — в шапке лендинга, справа.
 *
 * Сессия — JWT в httpOnly cookie: страница её не читает и не хранит. Поэтому
 * после перезагрузки вход сохраняется, а виджет узнаёт о нём сам, спрашивая
 * `/auth/me`. Хосту достаточно дёрнуть `AKSWidget.configure()`, чтобы фрейм
 * перечитал сессию.
 */

type Session = {
  /** Логин, который ввели: именно его показываем в шапке. */
  login: string
  name: string
  role: string
}

type User = { id: string; name: string; role: string }
type MeResponse = { user: User; request_id: string }

declare global {
  interface Window {
    AKS_WIDGET?: { apiUrl?: string; kbId?: string }
    AKSWidget?: { configure?: (next: Record<string, unknown>) => void }
  }
}

const API_URL = (window.AKS_WIDGET?.apiUrl || "/api/v1").replace(/\/+$/, "")

const SPRING = { type: "spring", bounce: 0, duration: 0.28 } as const

const FIELD =
  "h-9 w-full rounded-xl border border-black/10 bg-background px-3 text-[13px] outline-none transition-shadow focus:border-brand/50 focus:ring-4 focus:ring-brand/15"

const DEMO_ACCOUNTS = [
  { label: "Клиент", login: "user" },
  { label: "Сотрудник", login: "employee" },
] as const

/**
 * `crypto.randomUUID` есть только в secure context (HTTPS/localhost): по
 * обычному http вход падал с `TypeError` и ловился как «Сервис недоступен».
 * Фолбэк тот же, что в виджете (`shared/db/chats.ts`).
 */
function requestId(): string {
  if (typeof crypto !== "undefined" && crypto.randomUUID) {
    return crypto.randomUUID()
  }
  return `${Date.now().toString(16)}-${Math.random().toString(16).slice(2, 10)}`
}

/**
 * Лоадер виджета подключается асинхронно, поэтому `AKSWidget` может появиться
 * уже после входа. Несколько коротких попыток надёжнее, чем гонка при загрузке.
 */
function notifyWidget() {
  let attempts = 0

  function send() {
    if (window.AKSWidget?.configure) {
      window.AKSWidget.configure({})
      return
    }
    if (attempts++ < 50) window.setTimeout(send, 100)
  }

  send()
}

async function readSession(): Promise<Session | null> {
  try {
    const response = await fetch(`${API_URL}/auth/me`, { credentials: "include" })
    if (!response.ok) return null
    const payload = (await response.json()) as MeResponse
    return { login: payload.user.name, name: payload.user.name, role: payload.user.role }
  } catch {
    return null
  }
}

export function AuthMenu() {
  const [session, setSession] = useState<Session | null>(null)
  const [open, setOpen] = useState(false)
  const [login, setLogin] = useState("")
  const [password, setPassword] = useState("")
  const [pending, setPending] = useState(false)
  const [error, setError] = useState<string | null>(null)

  // Cookie переживает перезагрузку — восстанавливаем шапку по ней.
  useEffect(() => {
    void readSession().then(setSession)
  }, [])

  useEffect(() => {
    notifyWidget()
  }, [session])

  async function signIn(withLogin: string, withPassword: string) {
    if (!withLogin.trim() || !withPassword) return

    setLogin(withLogin)
    setPassword(withPassword)
    setPending(true)
    setError(null)

    try {
      const response = await fetch(`${API_URL}/auth/login`, {
        method: "POST",
        credentials: "include",
        headers: {
          "Content-Type": "application/json",
          "X-Request-ID": requestId(),
        },
        body: JSON.stringify({ username: withLogin, password: withPassword }),
      })
      const payload = (await response.json().catch(() => null)) as MeResponse | null

      if (!response.ok || !payload?.user) {
        setError(
          response.status === 401
            ? "Неверный логин или пароль."
            : "Не удалось войти. Попробуйте ещё раз.",
        )
        return
      }

      setSession({
        login: withLogin,
        name: payload.user.name,
        role: payload.user.role,
      })
      setOpen(false)
      setPassword("")
    } catch {
      setError("Сервис недоступен. Проверьте соединение.")
    } finally {
      setPending(false)
    }
  }

  async function signOut() {
    setSession(null)
    setLogin("")
    setPassword("")
    setError(null)
    try {
      await fetch(`${API_URL}/auth/logout`, {
        method: "POST",
        credentials: "include",
        headers: { "X-Request-ID": requestId() },
      })
    } catch {
      // Cookie уже не нужна: сессия на экране закрыта в любом случае.
    }
  }

  return (
    <div className="relative">
      {session ? (
        <div className="flex items-center gap-1">
          <span className="max-w-[9rem] truncate rounded-full bg-black/[0.06] px-3 py-1.5 text-[13px] font-medium">
            {session.login}
          </span>
          <button
            type="button"
            onClick={() => void signOut()}
            className="rounded-full px-3 py-1.5 text-[13px] font-medium text-brand transition-colors hover:bg-black/[0.04] active:opacity-70"
          >
            Выйти
          </button>
        </div>
      ) : (
        <div className="relative z-50 flex items-center gap-1.5">
          {/* Демо-вход в одно нажатие: креды уходят как есть, без заполнения формы. */}
          {DEMO_ACCOUNTS.map((account) => (
            <button
              key={account.login}
              type="button"
              disabled={pending}
              onClick={() => {
                // Форма открывается: в ней видны прогресс и текст ошибки.
                setOpen(true)
                void signIn(account.login, account.login)
              }}
              className="rounded-full border border-black/10 px-3 py-1.5 text-[13px] font-medium transition-colors hover:bg-black/[0.04] disabled:opacity-50 active:opacity-70"
            >
              {account.label}
            </button>
          ))}
          <button
            type="button"
            aria-expanded={open}
            aria-haspopup="dialog"
            onClick={() => setOpen((value) => !value)}
            className="rounded-full border border-black/10 px-4 py-1.5 text-[13px] font-medium transition-colors hover:bg-black/[0.04] active:opacity-70"
          >
            Войти
          </button>
        </div>
      )}

      <AnimatePresence>
        {open && !session ? (
          <>
            {/* Клик мимо формы закрывает её, не трогая разметку шапки. */}
            <div
              className="fixed inset-0 z-40"
              aria-hidden
              onClick={() => setOpen(false)}
            />
            <motion.form
              initial={{ opacity: 0, y: -6, scale: 0.98 }}
              animate={{ opacity: 1, y: 0, scale: 1 }}
              exit={{ opacity: 0, y: -6, scale: 0.98 }}
              transition={SPRING}
              className="absolute top-full right-0 z-50 mt-2 w-[268px] rounded-2xl border border-black/10 bg-background/95 p-4 shadow-[0_18px_50px_rgba(0,0,0,0.18)] backdrop-blur-xl"
              onSubmit={(event) => {
                event.preventDefault()
                void signIn(login, password)
              }}
            >
              <p className="text-[13px] font-semibold">Вход в базу знаний</p>
              <p className="mt-1 text-[12px] leading-snug text-black/50">
                Клиент видит каталог, сотрудник — ещё и внутренние документы.
              </p>

              <div className="mt-3 space-y-2">
                <input
                  className={FIELD}
                  value={login}
                  autoComplete="username"
                  aria-label="Логин"
                  placeholder="Логин"
                  onChange={(event) => setLogin(event.target.value)}
                />
                <input
                  className={FIELD}
                  type="password"
                  value={password}
                  autoComplete="current-password"
                  aria-label="Пароль"
                  placeholder="Пароль"
                  onChange={(event) => setPassword(event.target.value)}
                />
              </div>

              {error ? (
                <p role="alert" className="mt-2 text-[12px] text-red-600">
                  {error}
                </p>
              ) : null}

              <button
                type="submit"
                disabled={pending || !login.trim() || !password}
                className="mt-3 h-9 w-full rounded-xl bg-brand text-[13px] font-medium text-white transition-colors hover:bg-brand/85 disabled:opacity-50"
              >
                {pending ? "Входим…" : "Войти"}
              </button>
            </motion.form>
          </>
        ) : null}
      </AnimatePresence>
    </div>
  )
}
