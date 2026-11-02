import type { LoginUser, MeResponse } from "@/shared/types/api"
import { request } from "./http"

export type AuthOptions = {
  apiUrl?: string
  signal?: AbortSignal
}

/**
 * Текущий пользователь. `null` вместо ошибки на 401: гость — это состояние,
 * а не сбой.
 */
export async function fetchSession(options: AuthOptions = {}): Promise<LoginUser | null> {
  try {
    const payload = await request<MeResponse>("/auth/me", {
      apiUrl: options.apiUrl,
      signal: options.signal,
    })
    return payload.user
  } catch (error) {
    if (isStatus(error, 401)) return null
    throw error
  }
}

export async function signIn(
  username: string,
  password: string,
  options: AuthOptions = {},
): Promise<LoginUser> {
  const payload = await request<MeResponse>("/auth/login", {
    method: "POST",
    body: { username, password },
    apiUrl: options.apiUrl,
    signal: options.signal,
  })
  return payload.user
}

/** Сервер удаляет cookie; сам JWT доживает свой срок. */
export function signOut(options: AuthOptions = {}): Promise<unknown> {
  return request<unknown>("/auth/logout", {
    method: "POST",
    apiUrl: options.apiUrl,
    signal: options.signal,
  })
}

function isStatus(error: unknown, status: number): boolean {
  return (
    typeof error === "object" &&
    error !== null &&
    "status" in error &&
    (error as { status: unknown }).status === status
  )
}
