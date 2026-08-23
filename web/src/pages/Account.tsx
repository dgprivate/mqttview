import { useState } from 'react'
import { api } from '../api/client'
import { useAuth } from '../auth/AuthContext'
import { Alert, formatRelative } from '../components/common'
import { TwoFactorCard } from './TwoFactorCard'

export function Account() {
  const { user } = useAuth()
  const [current, setCurrent] = useState('')
  const [next, setNext] = useState('')
  const [confirm, setConfirm] = useState('')
  const [status, setStatus] = useState<{ kind: 'error' | 'ok'; text: string } | null>(null)
  const [busy, setBusy] = useState(false)

  if (!user) return null
  const ssoOnly = user.provider !== 'local'
  // The stored id is "homeassistant"; nobody writes it that way.
  const providerName = user.provider === 'homeassistant' ? 'Home Assistant' : user.provider

  async function submit(event: React.FormEvent) {
    event.preventDefault()
    setStatus(null)
    if (next !== confirm) {
      setStatus({ kind: 'error', text: 'The new passwords do not match.' })
      return
    }
    setBusy(true)
    try {
      await api.changePassword(current, next)
      setCurrent('')
      setNext('')
      setConfirm('')
      setStatus({ kind: 'ok', text: 'Password changed. Your other sessions were signed out.' })
    } catch (err) {
      setStatus({ kind: 'error', text: err instanceof Error ? err.message : 'Could not change the password' })
    } finally {
      setBusy(false)
    }
  }

  return (
    <>
      <div className="page-head">
        <div>
          <h1>Your account</h1>
          <p className="subtitle">
            {user.email} · role {user.role} · signed in with {providerName} · last seen{' '}
            {formatRelative(user.lastLoginAt)}
          </p>
        </div>
      </div>

      <TwoFactorCard ssoOnly={ssoOnly} provider={user.provider} />

      <div className="card">
        <h2>Change password</h2>

        {ssoOnly ? (
          <Alert kind="info">
            This account signs in through {providerName}, so its password is managed there.
          </Alert>
        ) : (
          <>
            {status && <Alert kind={status.kind}>{status.text}</Alert>}
            <form onSubmit={submit} style={{ maxWidth: '28rem' }}>
              <div className="field">
                <label htmlFor="current">Current password</label>
                <input
                  id="current"
                  type="password"
                  autoComplete="current-password"
                  value={current}
                  onChange={(e) => setCurrent(e.target.value)}
                  required
                />
              </div>
              <div className="field">
                <label htmlFor="next">New password (at least 10 characters)</label>
                <input
                  id="next"
                  type="password"
                  autoComplete="new-password"
                  minLength={10}
                  value={next}
                  onChange={(e) => setNext(e.target.value)}
                  required
                />
              </div>
              <div className="field">
                <label htmlFor="confirm">Confirm new password</label>
                <input
                  id="confirm"
                  type="password"
                  autoComplete="new-password"
                  minLength={10}
                  value={confirm}
                  onChange={(e) => setConfirm(e.target.value)}
                  required
                />
              </div>
              <button type="submit" className="primary" disabled={busy}>
                {busy ? <span className="spinner" /> : 'Change password'}
              </button>
            </form>
          </>
        )}
      </div>
    </>
  )
}
