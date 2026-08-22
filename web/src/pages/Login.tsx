import { useEffect, useState } from 'react'
import { api, ApiError } from '../api/client'
import type { AuthConfig } from '../api/types'
import { useAuth } from '../auth/AuthContext'
import { Alert, Spinner } from '../components/common'

export function Login() {
  const { signIn } = useAuth()
  const [config, setConfig] = useState<AuthConfig | null>(null)
  const [email, setEmail] = useState('')
  const [password, setPassword] = useState('')
  const [code, setCode] = useState('')
  // Set once the server has said the password was right but not sufficient.
  const [needCode, setNeedCode] = useState(false)
  const [error, setError] = useState('')
  const [busy, setBusy] = useState(false)

  useEffect(() => {
    // An error in the query string comes from a failed SSO round trip.
    const params = new URLSearchParams(window.location.search)
    const ssoError = params.get('error')
    if (ssoError) setError(ssoError)

    api.authConfig().then(setConfig).catch((err: Error) => setError(err.message))
  }, [])

  async function submit(event: React.FormEvent) {
    event.preventDefault()
    setError('')
    setBusy(true)
    try {
      await signIn(email, password, code)
      setNeedCode(false)
    } catch (err) {
      // The server distinguishes "wrong password" from "now show me a code",
      // so the form can ask for the second factor without implying the first
      // was wrong.
      const wantsCode = err instanceof ApiError && err.twoFactorRequired
      setNeedCode(wantsCode)
      if (wantsCode && !code) {
        setError('')
      } else {
        setError(err instanceof Error ? err.message : 'Sign in failed')
      }
      setCode('')
    } finally {
      setBusy(false)
    }
  }

  if (!config) {
    return (
      <div className="login-wrap">
        <Spinner />
      </div>
    )
  }

  return (
    <div className="login-wrap">
      <div className="login-card">
        <h1>
          mqtt<span style={{ color: 'var(--accent)' }}>view</span>
        </h1>

        <Alert kind="error">{error}</Alert>

        {config.needsBootstrap && (
          <Alert kind="info">
            No accounts exist yet. The first administrator's credentials were printed in the server
            log at startup.
          </Alert>
        )}

        {config.allowLocal && (
          <form onSubmit={submit}>
            <div className="field">
              <label htmlFor="email">Email</label>
              <input
                id="email"
                type="email"
                autoComplete="username"
                inputMode="email"
                value={email}
                onChange={(e) => setEmail(e.target.value)}
                required
              />
            </div>
            <div className="field">
              <label htmlFor="password">Password</label>
              <input
                id="password"
                type="password"
                autoComplete="current-password"
                value={password}
                onChange={(e) => setPassword(e.target.value)}
                required
              />
            </div>
            {needCode && (
              <div className="field">
                <label htmlFor="code">Authentication code</label>
                <input
                  id="code"
                  // Not type="number": a leading zero matters and the spinner
                  // arrows are useless here.
                  type="text"
                  inputMode="numeric"
                  autoComplete="one-time-code"
                  autoFocus
                  placeholder="6 digits, or a recovery code"
                  value={code}
                  onChange={(e) => setCode(e.target.value)}
                  required
                />
                <small>
                  From your authenticator app. If you have lost it, enter one of the recovery codes you saved.
                </small>
              </div>
            )}
            <button type="submit" className="primary" style={{ width: '100%' }} disabled={busy}>
              {busy ? <span className="spinner" /> : needCode ? 'Verify' : 'Sign in'}
            </button>
          </form>
        )}

        {config.allowLocal && config.providers.length > 0 && <div className="divider">or</div>}

        <div className="stack">
          {config.providers.map((p) => (
            <a
              key={p.id}
              className="btn"
              style={{ width: '100%' }}
              href={`/api/auth/sso/${p.id}/start?next=${encodeURIComponent(window.location.pathname)}`}
            >
              Continue with {p.displayName}
            </a>
          ))}
        </div>

        {!config.allowLocal && config.providers.length === 0 && (
          <Alert kind="error">
            No sign-in method is enabled. Check the auth section of the server configuration.
          </Alert>
        )}
      </div>
    </div>
  )
}
