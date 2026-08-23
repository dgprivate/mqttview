import { createContext, useCallback, useContext, useEffect, useMemo, useState } from 'react'
import type { ReactNode } from 'react'
import { api, ApiError } from '../api/client'
import type { AuthMode, Role, User } from '../api/types'
import { liveSocket } from '../ws/socket'

interface AuthState {
  user: User | null
  loading: boolean
  /**
   * mode is what the server said about who authenticates. Pages use it to stop
   * offering things that do not exist in Home Assistant mode — a login form, a
   * sign-out link, a password to change.
   */
  mode: AuthMode
  signIn: (email: string, password: string, code?: string) => Promise<void>
  signOut: () => Promise<void>
  /** can reports whether the signed-in user holds at least the given role. */
  can: (role: Role) => boolean
}

const AuthContext = createContext<AuthState | null>(null)

const rank: Record<Role, number> = { viewer: 1, operator: 2, admin: 3 }

export function AuthProvider({ children }: { children: ReactNode }) {
  const [user, setUser] = useState<User | null>(null)
  const [mode, setMode] = useState<AuthMode>('standalone')
  const [loading, setLoading] = useState(true)

  useEffect(() => {
    let cancelled = false
    // The mode is asked for alongside the user rather than after it, because
    // the answer decides what to render when there is no user: a login form,
    // or an explanation that Home Assistant refused.
    Promise.allSettled([api.authConfig(), api.me()])
      .then(([cfg, me]) => {
        if (cancelled) return
        if (cfg.status === 'fulfilled') setMode(cfg.value.mode ?? 'standalone')
        if (me.status === 'fulfilled') {
          setUser(me.value)
          return
        }
        // A 401 just means "not signed in yet", not a failure worth surfacing.
        const err = me.reason
        if (!(err instanceof ApiError) || err.status !== 401) {
          console.error('could not load the current user', err)
        }
      })
      .finally(() => {
        if (!cancelled) setLoading(false)
      })
    return () => {
      cancelled = true
    }
  }, [])

  useEffect(() => {
    if (user) {
      liveSocket.connect()
      return () => liveSocket.disconnect()
    }
    return undefined
  }, [user])

  const signIn = useCallback(async (email: string, password: string, code = '') => {
    setUser(await api.login(email, password, code))
  }, [])

  const signOut = useCallback(async () => {
    try {
      await api.logout()
    } finally {
      liveSocket.disconnect()
      setUser(null)
    }
  }, [])

  const can = useCallback(
    (role: Role) => (user ? rank[user.role] >= rank[role] : false),
    [user],
  )

  const value = useMemo(
    () => ({ user, loading, mode, signIn, signOut, can }),
    [user, loading, mode, signIn, signOut, can],
  )

  return <AuthContext.Provider value={value}>{children}</AuthContext.Provider>
}

export function useAuth(): AuthState {
  const ctx = useContext(AuthContext)
  if (!ctx) throw new Error('useAuth must be used inside an AuthProvider')
  return ctx
}
