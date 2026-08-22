import { useCallback, useEffect, useState } from 'react'
import { api } from '../api/client'
import type { TwoFactorStatus } from '../api/types'
import { Alert, Spinner } from '../components/common'

/**
 * TwoFactorCard is the enrol / disable panel on the account page.
 *
 * The secret and the recovery codes are shown exactly once each, because only
 * their ciphertext and hashes are kept. Everything on screen that cannot be
 * retrieved again says so.
 */
export function TwoFactorCard({ ssoOnly, provider }: { ssoOnly: boolean; provider: string }) {
  const [status, setStatus] = useState<TwoFactorStatus | null>(null)
  const [enrolment, setEnrolment] = useState<{ secret: string; uri: string } | null>(null)
  const [codes, setCodes] = useState<string[] | null>(null)
  const [code, setCode] = useState('')
  const [password, setPassword] = useState('')
  const [error, setError] = useState('')
  const [busy, setBusy] = useState(false)

  const load = useCallback(async () => {
    try {
      setStatus(await api.twoFactorStatus())
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Could not read the two-factor status')
    }
  }, [])

  useEffect(() => {
    void load()
  }, [load])

  async function run(fn: () => Promise<void>) {
    setError('')
    setBusy(true)
    try {
      await fn()
    } catch (err) {
      setError(err instanceof Error ? err.message : 'That did not work')
    } finally {
      setBusy(false)
    }
  }

  if (ssoOnly) {
    return (
      <div className="card">
        <h2>Two-factor authentication</h2>
        <Alert kind="info">
          This account signs in through {provider}. A second factor belongs at that identity provider, where it
          protects every application at once rather than this one.
        </Alert>
      </div>
    )
  }

  if (!status) return <div className="card">{error ? <Alert kind="error">{error}</Alert> : <Spinner />}</div>

  return (
    <div className="card">
      <div className="card-head">
        <h2>Two-factor authentication</h2>
        <span className={`badge ${status.enabled ? 'ok' : ''}`}>{status.enabled ? 'on' : 'off'}</span>
      </div>

      {error && <Alert kind="error">{error}</Alert>}
      {status.requiredForThisUser && !status.enabled && (
        <Alert kind="error">This server requires two-factor authentication. Set it up to keep your access.</Alert>
      )}

      {/* Shown once, right after confirming. There is no way to fetch them again. */}
      {codes && (
        <Alert kind="ok">
          <strong>Save these recovery codes now.</strong> Each works once, in place of your authenticator. They are
          stored hashed, so this is the only time they can be displayed.
          <ul className="plc-facts" style={{ marginTop: '0.75rem' }}>
            {codes.map((c) => (
              <li key={c} className="mono">
                {c}
              </li>
            ))}
          </ul>
          <button className="btn" onClick={() => setCodes(null)}>
            I have saved them
          </button>
        </Alert>
      )}

      {!status.enabled && !enrolment && (
        <>
          <p className="subtitle">
            An authenticator app generates a six-digit code that changes every thirty seconds. Signing in will need
            your password and that code.
          </p>
          <button
            className="btn primary"
            disabled={busy}
            onClick={() => run(async () => setEnrolment(await api.twoFactorEnrol()))}
          >
            Set up
          </button>
        </>
      )}

      {!status.enabled && enrolment && (
        <>
          <p className="subtitle">
            Scan this with your authenticator app, or type the key in by hand. Then enter the code it shows to
            prove it worked.
          </p>
          <dl className="plc-facts">
            <div>
              <dt>Setup key</dt>
              <dd className="mono">{enrolment.secret}</dd>
            </div>
          </dl>
          <p className="subtitle mono" style={{ overflowWrap: 'anywhere' }}>
            {enrolment.uri}
          </p>
          <div className="field" style={{ maxWidth: '20rem' }}>
            <label htmlFor="tfa-confirm">Code from the app</label>
            <input
              id="tfa-confirm"
              type="text"
              inputMode="numeric"
              autoComplete="one-time-code"
              value={code}
              onChange={(e) => setCode(e.target.value)}
            />
          </div>
          <div className="button-row">
            <button
              className="btn primary"
              disabled={busy || code.length < 6}
              onClick={() =>
                run(async () => {
                  const result = await api.twoFactorConfirm(code)
                  setCodes(result.recoveryCodes)
                  setEnrolment(null)
                  setCode('')
                  await load()
                })
              }
            >
              Turn on
            </button>
            <button className="btn" disabled={busy} onClick={() => setEnrolment(null)}>
              Cancel
            </button>
          </div>
        </>
      )}

      {status.enabled && (
        <>
          <dl className="plc-facts">
            <div>
              <dt>Recovery codes left</dt>
              <dd className={status.recoveryCodesLeft <= 2 ? 'warn' : ''}>{status.recoveryCodesLeft} of 10</dd>
            </div>
          </dl>

          <div className="field" style={{ maxWidth: '20rem' }}>
            <label htmlFor="tfa-code">Current code</label>
            <input
              id="tfa-code"
              type="text"
              inputMode="numeric"
              autoComplete="one-time-code"
              value={code}
              onChange={(e) => setCode(e.target.value)}
            />
          </div>

          <div className="button-row">
            <button
              className="btn"
              disabled={busy || !code}
              onClick={() =>
                run(async () => {
                  const result = await api.regenerateRecoveryCodes(code)
                  setCodes(result.recoveryCodes)
                  setCode('')
                  await load()
                })
              }
            >
              New recovery codes
            </button>
          </div>

          {!status.requiredForThisUser && (
            <>
              <p className="subtitle" style={{ marginTop: '1rem' }}>
                Turning it off needs your password as well as a code, because an unlocked screen is exactly what
                somebody else would use to do this.
              </p>
              <div className="field" style={{ maxWidth: '20rem' }}>
                <label htmlFor="tfa-password">Password</label>
                <input
                  id="tfa-password"
                  type="password"
                  autoComplete="current-password"
                  value={password}
                  onChange={(e) => setPassword(e.target.value)}
                />
              </div>
              <button
                className="btn"
                disabled={busy || !code || !password}
                onClick={() =>
                  run(async () => {
                    await api.twoFactorDisable(password, code)
                    setPassword('')
                    setCode('')
                    await load()
                  })
                }
              >
                Turn off
              </button>
            </>
          )}
        </>
      )}
    </div>
  )
}
