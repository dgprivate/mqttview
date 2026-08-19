import { useCallback, useEffect, useState } from 'react'
import { api } from '../api/client'
import type { Role, User } from '../api/types'
import { useAuth } from '../auth/AuthContext'
import { Alert, formatRelative, Spinner } from '../components/common'

export function Users() {
  const { user: me } = useAuth()
  const [users, setUsers] = useState<User[] | null>(null)
  const [error, setError] = useState('')
  const [ok, setOk] = useState('')
  const [form, setForm] = useState({ email: '', name: '', role: 'viewer' as Role, password: '' })
  const [busy, setBusy] = useState(false)

  const load = useCallback(async () => {
    try {
      setUsers(await api.users())
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Could not load users')
    }
  }, [])

  useEffect(() => {
    void load()
  }, [load])

  async function create(event: React.FormEvent) {
    event.preventDefault()
    setError('')
    setOk('')
    setBusy(true)
    try {
      await api.createUser(form)
      setForm({ email: '', name: '', role: 'viewer', password: '' })
      setOk('User created.')
      await load()
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Could not create the user')
    } finally {
      setBusy(false)
    }
  }

  async function update(u: User, changes: Partial<User>) {
    setError('')
    setOk('')
    try {
      await api.updateUser(u.id, {
        email: changes.email ?? u.email,
        name: changes.name ?? u.name,
        role: changes.role ?? u.role,
        disabled: changes.disabled ?? u.disabled,
      })
      await load()
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Could not update the user')
    }
  }

  async function remove(u: User) {
    if (!window.confirm(`Delete ${u.email}?`)) return
    setError('')
    try {
      await api.deleteUser(u.id)
      await load()
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Could not delete the user')
    }
  }

  async function resetPassword(u: User) {
    const password = window.prompt(`New password for ${u.email} (at least 10 characters)`)
    if (!password) return
    setError('')
    setOk('')
    try {
      await api.resetPassword(u.id, password)
      setOk(`Password reset for ${u.email}. Their other sessions were signed out.`)
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Could not reset the password')
    }
  }

  if (!users) return error ? <Alert kind="error">{error}</Alert> : <Spinner />

  return (
    <>
      <div className="page-head">
        <div>
          <h1>Users</h1>
          <p className="subtitle">
            Viewers read, operators publish and control, administrators manage everything.
          </p>
        </div>
      </div>

      <Alert kind="error">{error}</Alert>
      <Alert kind="ok">{ok}</Alert>

      <div className="card">
        <div className="table-wrap">
          <table className="responsive">
            <thead>
              <tr>
                <th>Email</th>
                <th>Name</th>
                <th>Role</th>
                <th>Sign-in</th>
                <th>Last seen</th>
                <th />
              </tr>
            </thead>
            <tbody>
              {users.map((u) => (
                <tr key={u.id}>
                  <td data-label="Email">
                    {u.email}
                    {u.disabled && <span className="badge err"> disabled</span>}
                    {u.id === me?.id && <span className="badge"> you</span>}
                  </td>
                  <td data-label="Name">{u.name || '—'}</td>
                  <td data-label="Role">
                    <select
                      value={u.role}
                      onChange={(e) => void update(u, { role: e.target.value as Role })}
                      aria-label={`Role for ${u.email}`}
                    >
                      <option value="viewer">viewer</option>
                      <option value="operator">operator</option>
                      <option value="admin">admin</option>
                    </select>
                  </td>
                  <td data-label="Sign-in">{u.provider}</td>
                  <td data-label="Last seen">{formatRelative(u.lastLoginAt)}</td>
                  <td data-label="Actions">
                    <div className="button-row">
                      <button className="small" onClick={() => void resetPassword(u)}>
                        Set password
                      </button>
                      {u.id !== me?.id && (
                        <>
                          <button className="small" onClick={() => void update(u, { disabled: !u.disabled })}>
                            {u.disabled ? 'Enable' : 'Disable'}
                          </button>
                          <button className="small danger" onClick={() => void remove(u)}>
                            Delete
                          </button>
                        </>
                      )}
                    </div>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      </div>

      <div className="card">
        <h2>Add a user</h2>
        <p className="subtitle">
          Leave the password blank to pre-authorise someone who will sign in through SSO.
        </p>
        <form onSubmit={create}>
          <div className="field-row two">
            <div className="field">
              <label htmlFor="new-email">Email</label>
              <input
                id="new-email"
                type="email"
                inputMode="email"
                value={form.email}
                onChange={(e) => setForm({ ...form, email: e.target.value })}
                required
              />
            </div>
            <div className="field">
              <label htmlFor="new-name">Name</label>
              <input
                id="new-name"
                value={form.name}
                onChange={(e) => setForm({ ...form, name: e.target.value })}
              />
            </div>
          </div>
          <div className="field-row two">
            <div className="field">
              <label htmlFor="new-role">Role</label>
              <select
                id="new-role"
                value={form.role}
                onChange={(e) => setForm({ ...form, role: e.target.value as Role })}
              >
                <option value="viewer">viewer</option>
                <option value="operator">operator</option>
                <option value="admin">admin</option>
              </select>
            </div>
            <div className="field">
              <label htmlFor="new-password">Password (optional)</label>
              <input
                id="new-password"
                type="password"
                autoComplete="new-password"
                value={form.password}
                onChange={(e) => setForm({ ...form, password: e.target.value })}
              />
            </div>
          </div>
          <button type="submit" className="primary" disabled={busy}>
            {busy ? <span className="spinner" /> : 'Create user'}
          </button>
        </form>
      </div>
    </>
  )
}
