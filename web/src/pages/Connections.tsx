import { useCallback, useEffect, useState } from 'react'
import { Link } from 'react-router-dom'
import { api } from '../api/client'
import type { Connection } from '../api/types'
import { useAuth } from '../auth/AuthContext'
import { Alert, Empty, Spinner, StatusBadge } from '../components/common'
import { useConnectionStatus } from '../ws/socket'

export function Connections() {
  const { can } = useAuth()
  const [connections, setConnections] = useState<Connection[] | null>(null)
  const [error, setError] = useState('')
  const [busy, setBusy] = useState<string | null>(null)
  const liveStatus = useConnectionStatus()

  const load = useCallback(async () => {
    try {
      setConnections(await api.connections())
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Could not load connections')
    }
  }, [])

  useEffect(() => {
    void load()
  }, [load])

  async function act(id: string, action: 'connect' | 'disconnect' | 'delete') {
    setError('')
    setBusy(id)
    try {
      if (action === 'connect') await api.connect(id)
      else if (action === 'disconnect') await api.disconnect(id)
      else {
        if (!window.confirm('Delete this connection and everything mqttview learned from it?')) {
          return
        }
        await api.deleteConnection(id)
      }
      await load()
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Action failed')
    } finally {
      setBusy(null)
    }
  }

  if (!connections) return <Spinner />

  return (
    <>
      <div className="page-head">
        <div>
          <h1>Connections</h1>
          <p className="subtitle">Brokers mqttview knows about.</p>
        </div>
        {can('admin') && (
          <Link className="btn primary" to="/connections/new">
            Add broker
          </Link>
        )}
      </div>

      <Alert kind="error">{error}</Alert>

      {connections.length === 0 ? (
        <Empty>
          No brokers yet.{' '}
          {can('admin') ? (
            <Link to="/connections/new">Add one</Link>
          ) : (
            'Ask an administrator to add one.'
          )}
        </Empty>
      ) : (
        <div className="card">
          <div className="table-wrap">
            <table className="responsive">
              <thead>
                <tr>
                  <th>Name</th>
                  <th>Broker</th>
                  <th>MQTT</th>
                  <th>Status</th>
                  <th>Topics</th>
                  <th>Messages</th>
                  <th />
                </tr>
              </thead>
              <tbody>
                {connections.map((c) => {
                  const status = liveStatus[c.id] ?? c.status
                  const connected = status.state === 'connected'
                  return (
                    <tr key={c.id}>
                      <td data-label="Name">
                        <Link to={`/connections/${c.id}`}>{c.name}</Link>
                      </td>
                      <td data-label="Broker" className="mono">
                        {c.url}
                      </td>
                      <td data-label="MQTT">{c.version}</td>
                      <td data-label="Status">
                        <StatusBadge status={status} />
                      </td>
                      <td data-label="Topics">{c.topics.toLocaleString()}</td>
                      <td data-label="Messages">{status.received.toLocaleString()}</td>
                      <td data-label="Actions">
                        <div className="button-row">
                          {can('operator') && (
                            <button
                              className="small"
                              disabled={busy === c.id}
                              onClick={() => void act(c.id, connected ? 'disconnect' : 'connect')}
                            >
                              {connected ? 'Disconnect' : 'Connect'}
                            </button>
                          )}
                          {can('admin') && (
                            <>
                              <Link className="btn small" to={`/connections/${c.id}/edit`}>
                                Edit
                              </Link>
                              <button
                                className="small danger"
                                disabled={busy === c.id}
                                onClick={() => void act(c.id, 'delete')}
                              >
                                Delete
                              </button>
                            </>
                          )}
                        </div>
                      </td>
                    </tr>
                  )
                })}
              </tbody>
            </table>
          </div>
        </div>
      )}

      {connections.some((c) => (liveStatus[c.id] ?? c.status).lastError) && (
        <div className="card">
          <h2>Recent connection errors</h2>
          {connections.map((c) => {
            const status = liveStatus[c.id] ?? c.status
            if (!status.lastError) return null
            return (
              <p key={c.id} className="subtitle">
                <strong>{c.name}:</strong> {status.lastError}
              </p>
            )
          })}
        </div>
      )}
    </>
  )
}
