import { createContext, useCallback, useContext, useEffect, useMemo, useState } from 'react'
import type { ReactNode } from 'react'
import { api, ApiError } from '../api/client'
import type { Role, User } from '../api/types'
import { liveSocket } from '../ws/socket'

interface AuthState {
  user: User | null
  loading: boolean
  signIn: (email: string, password: string, code?: string) => Promise<void>
  signOut: () => Promise<void>
  /** can reports whether the signed-in user holds at least the given role. */
  can: (role: Role) => boolean
}

const AuthContext = createContext<AuthState | null>(null)

const rank: Record<Role, number> = { viewer: 1, operator: 2, admin: 3 }

export function AuthProvider({ children }: { children: ReactNode }) {
  const [user, setUser] = useState<User | null>(null)
  const [loading, setLoading] = useState(true)

  useEffect(() => {
    let cancelled = false
    api
      .me()
      .then((u) => {
        if (!cancelled) setUser(u)
      })
      .catch((err: unknown) => {
        // A 401 here just means "not signed in yet", not a failure worth
        // surfacing.
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
    () => ({ user, loading, signIn, signOut, can }),
    [user, loading, signIn, signOut, can],
  )

  return <AuthContext.Provider value={value}>{children}</AuthContext.Provider>
}

export function useAuth(): AuthState {
  const ctx = useContext(AuthContext)
  if (!ctx) throw new Error('useAuth must be used inside an AuthProvider')
  return ctx
}
