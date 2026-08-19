import { useEffect, useState } from 'react'
import { useNavigate, useParams } from 'react-router-dom'
import { api } from '../api/client'
import type { Connection, ConnectionInput, Subscription } from '../api/types'
import { Alert, Spinner } from '../components/common'

const emptyInput: ConnectionInput = {
  name: '',
  url: 'mqtt://localhost:1883',
  version: '3.1.1',
  clientId: '',
  username: '',
  password: '',
  keepAlive: 60,
  cleanStart: true,
  sessionExpiry: 0,
  tls: {
    insecureSkipVerify: false,
    serverName: '',
    minVersion: '1.2',
    alpn: null,
    caPem: '',
    clientCertPem: '',
    clientKeyPem: '',
  },
  will: null,
  subscriptions: [{ filter: '#', qos: 0 }],
  autoConnect: true,
  historySize: 0,
}

/**
 * ConnectionForm creates and edits brokers. On edit, secret fields start
 * blank and are only sent when the user types something — that is how an
 * unchanged password survives a save without ever being sent to the browser.
 */
export function ConnectionForm() {
  const { id } = useParams<{ id: string }>()
  const navigate = useNavigate()
  const editing = Boolean(id)

  const [input, setInput] = useState<ConnectionInput>(emptyInput)
  const [existing, setExisting] = useState<Connection | null>(null)
  const [loading, setLoading] = useState(editing)
  const [error, setError] = useState('')
  const [busy, setBusy] = useState(false)
  const [showTls, setShowTls] = useState(false)

  useEffect(() => {
    if (!id) return
    api
      .connection(id)
      .then((c) => {
        setExisting(c)
        setInput({
          name: c.name,
          url: c.url,
          version: c.version,
          clientId: c.clientId,
          username: c.username,
          password: null,
          keepAlive: c.keepAlive,
          cleanStart: c.cleanStart,
          sessionExpiry: c.sessionExpiry,
          tls: {
            insecureSkipVerify: c.tls.insecureSkipVerify,
            serverName: c.tls.serverName ?? '',
            minVersion: c.tls.minVersion ?? '1.2',
            alpn: c.tls.alpn ?? null,
            caPem: null,
            clientCertPem: null,
            clientKeyPem: null,
          },
          will: c.will ?? null,
          subscriptions: c.subscriptions.length ? c.subscriptions : [],
          autoConnect: c.autoConnect,
          historySize: c.historySize,
        })
        setShowTls(c.url.startsWith('ssl://') || c.url.startsWith('wss://'))
      })
      .catch((err: Error) => setError(err.message))
      .finally(() => setLoading(false))
  }, [id])

  function patch(changes: Partial<ConnectionInput>) {
    setInput((prev) => ({ ...prev, ...changes }))
  }

  function patchTls(changes: Partial<ConnectionInput['tls']>) {
    setInput((prev) => ({ ...prev, tls: { ...prev.tls, ...changes } }))
  }

  function setSubscription(index: number, changes: Partial<Subscription>) {
    setInput((prev) => ({
      ...prev,
      subscriptions: prev.subscriptions.map((s, i) => (i === index ? { ...s, ...changes } : s)),
    }))
  }

  async function submit(event: React.FormEvent) {
    event.preventDefault()
    setError('')
    setBusy(true)

    // Blank secret fields mean "leave the stored value alone" when editing,
    // and "no value" when creating.
    const payload: ConnectionInput = {
      ...input,
      password: input.password ? input.password : editing ? null : '',
      tls: {
        ...input.tls,
        caPem: input.tls.caPem ? input.tls.caPem : editing ? null : '',
        clientCertPem: input.tls.clientCertPem ? input.tls.clientCertPem : editing ? null : '',
        clientKeyPem: input.tls.clientKeyPem ? input.tls.clientKeyPem : editing ? null : '',
        alpn: input.tls.alpn && input.tls.alpn.length ? input.tls.alpn : null,
      },
      subscriptions: input.subscriptions.filter((s) => s.filter.trim() !== ''),
    }

    try {
      const saved = editing ? await api.updateConnection(id!, payload) : await api.createConnection(payload)
      navigate(`/connections/${saved.id}`)
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Could not save the connection')
    } finally {
      setBusy(false)
    }
  }

  if (loading) return <Spinner />

  const isTls = input.url.startsWith('ssl://') || input.url.startsWith('mqtts://') || input.url.startsWith('wss://')

  return (
    <>
      <div className="page-head">
        <div>
          <h1>{editing ? `Edit ${existing?.name ?? 'broker'}` : 'Add a broker'}</h1>
          <p className="subtitle">
            Supports MQTT 3.1, 3.1.1 and 5.0 over TCP, TLS, WebSocket and secure WebSocket.
          </p>
        </div>
      </div>

      <Alert kind="error">{error}</Alert>

      <form onSubmit={submit}>
        <div className="card">
          <h2>Broker</h2>

          <div className="field">
            <label htmlFor="name">Display name</label>
            <input
              id="name"
              value={input.name}
              onChange={(e) => patch({ name: e.target.value })}
              placeholder="Home broker"
              required
            />
          </div>

          <div className="field-row two">
            <div className="field">
              <label htmlFor="url">Broker URL</label>
              <input
                id="url"
                className="mono"
                value={input.url}
                onChange={(e) => patch({ url: e.target.value })}
                placeholder="mqtts://broker.example.com:8883"
                required
              />
              <p className="subtitle" style={{ marginTop: '0.25rem' }}>
                Schemes: mqtt, mqtts, ws, wss, unix. The port is optional.
              </p>
            </div>

            <div className="field">
              <label htmlFor="version">Protocol version</label>
              <select id="version" value={input.version} onChange={(e) => patch({ version: e.target.value })}>
                <option value="3.1">MQTT 3.1</option>
                <option value="3.1.1">MQTT 3.1.1</option>
                <option value="5.0">MQTT 5.0</option>
              </select>
            </div>
          </div>

          <div className="field-row two">
            <div className="field">
              <label htmlFor="username">Username</label>
              <input
                id="username"
                autoComplete="off"
                value={input.username}
                onChange={(e) => patch({ username: e.target.value })}
              />
            </div>
            <div className="field">
              <label htmlFor="password">
                Password{existing?.hasPassword ? ' (leave blank to keep the current one)' : ''}
              </label>
              <input
                id="password"
                type="password"
                autoComplete="new-password"
                value={input.password ?? ''}
                onChange={(e) => patch({ password: e.target.value })}
              />
            </div>
          </div>

          <div className="field-row three">
            <div className="field">
              <label htmlFor="clientId">Client ID (generated if empty)</label>
              <input
                id="clientId"
                className="mono"
                value={input.clientId}
                onChange={(e) => patch({ clientId: e.target.value })}
              />
            </div>
            <div className="field">
              <label htmlFor="keepAlive">Keep-alive (seconds)</label>
              <input
                id="keepAlive"
                type="number"
                inputMode="numeric"
                min={1}
                max={65535}
                value={input.keepAlive}
                onChange={(e) => patch({ keepAlive: Number(e.target.value) })}
              />
            </div>
            <div className="field">
              <label htmlFor="historySize">Message history (0 = default 2000)</label>
              <input
                id="historySize"
                type="number"
                inputMode="numeric"
                min={0}
                value={input.historySize}
                onChange={(e) => patch({ historySize: Number(e.target.value) })}
              />
            </div>
          </div>

          <div className="checkbox">
            <input
              id="cleanStart"
              type="checkbox"
              checked={input.cleanStart}
              onChange={(e) => patch({ cleanStart: e.target.checked })}
            />
            <label htmlFor="cleanStart">Start a clean session</label>
          </div>

          <div className="checkbox">
            <input
              id="autoConnect"
              type="checkbox"
              checked={input.autoConnect}
              onChange={(e) => patch({ autoConnect: e.target.checked })}
            />
            <label htmlFor="autoConnect">Connect automatically when mqttview starts</label>
          </div>

          {input.version === '5.0' && (
            <div className="field" style={{ maxWidth: '20rem' }}>
              <label htmlFor="sessionExpiry">Session expiry (seconds, MQTT 5)</label>
              <input
                id="sessionExpiry"
                type="number"
                inputMode="numeric"
                min={0}
                value={input.sessionExpiry}
                onChange={(e) => patch({ sessionExpiry: Number(e.target.value) })}
              />
            </div>
          )}
        </div>

        <div className="card">
          <div className="card-head">
            <h2>TLS</h2>
            <button type="button" className="small" onClick={() => setShowTls((v) => !v)}>
              {showTls ? 'Hide' : 'Show'}
            </button>
          </div>

          {!isTls && (
            <p className="subtitle">
              This broker URL is not encrypted. Use <code>mqtts://</code> or <code>wss://</code> to
              enable TLS.
            </p>
          )}

          {showTls && (
            <>
              <div className="field-row two">
                <div className="field">
                  <label htmlFor="serverName">Server name override (SNI)</label>
                  <input
                    id="serverName"
                    value={input.tls.serverName}
                    onChange={(e) => patchTls({ serverName: e.target.value })}
                  />
                </div>
                <div className="field">
                  <label htmlFor="minVersion">Minimum TLS version</label>
                  <select
                    id="minVersion"
                    value={input.tls.minVersion}
                    onChange={(e) => patchTls({ minVersion: e.target.value })}
                  >
                    <option value="1.2">TLS 1.2</option>
                    <option value="1.3">TLS 1.3</option>
                  </select>
                </div>
              </div>

              <div className="field">
                <label htmlFor="caPem">
                  CA certificate (PEM)
                  {existing?.tls.hasCa ? ' — one is stored; paste a new one to replace it' : ''}
                </label>
                <textarea
                  id="caPem"
                  value={input.tls.caPem ?? ''}
                  onChange={(e) => patchTls({ caPem: e.target.value })}
                  placeholder="-----BEGIN CERTIFICATE-----"
                />
              </div>

              <div className="field-row two">
                <div className="field">
                  <label htmlFor="clientCertPem">
                    Client certificate (PEM)
                    {existing?.tls.hasClientCert ? ' — stored' : ''}
                  </label>
                  <textarea
                    id="clientCertPem"
                    value={input.tls.clientCertPem ?? ''}
                    onChange={(e) => patchTls({ clientCertPem: e.target.value })}
                  />
                </div>
                <div className="field">
                  <label htmlFor="clientKeyPem">Client private key (PEM)</label>
                  <textarea
                    id="clientKeyPem"
                    value={input.tls.clientKeyPem ?? ''}
                    onChange={(e) => patchTls({ clientKeyPem: e.target.value })}
                  />
                </div>
              </div>

              <div className="checkbox">
                <input
                  id="insecure"
                  type="checkbox"
                  checked={input.tls.insecureSkipVerify}
                  onChange={(e) => patchTls({ insecureSkipVerify: e.target.checked })}
                />
                <label htmlFor="insecure">
                  Skip certificate verification (insecure — only for a self-signed broker you
                  control)
                </label>
              </div>
            </>
          )}
        </div>

        <div className="card">
          <div className="card-head">
            <h2>Subscriptions</h2>
            <button
              type="button"
              className="small"
              onClick={() => patch({ subscriptions: [...input.subscriptions, { filter: '', qos: 0 }] })}
            >
              Add filter
            </button>
          </div>

          {input.subscriptions.length === 0 && (
            <p className="subtitle">
              No subscriptions. Without at least one, mqttview will not receive any messages.
            </p>
          )}

          {input.subscriptions.map((sub, i) => (
            <div className="field-row two" key={i}>
              <div className="field">
                <label htmlFor={`filter-${i}`}>Topic filter</label>
                <input
                  id={`filter-${i}`}
                  className="mono"
                  value={sub.filter}
                  onChange={(e) => setSubscription(i, { filter: e.target.value })}
                  placeholder="home/+/temperature"
                />
              </div>
              <div className="field">
                <label htmlFor={`qos-${i}`}>QoS</label>
                <div style={{ display: 'flex', gap: '0.5rem' }}>
                  <select
                    id={`qos-${i}`}
                    value={sub.qos}
                    onChange={(e) => setSubscription(i, { qos: Number(e.target.value) })}
                  >
                    <option value={0}>0 — at most once</option>
                    <option value={1}>1 — at least once</option>
                    <option value={2}>2 — exactly once</option>
                  </select>
                  <button
                    type="button"
                    className="danger"
                    onClick={() =>
                      patch({ subscriptions: input.subscriptions.filter((_, idx) => idx !== i) })
                    }
                  >
                    Remove
                  </button>
                </div>
              </div>
            </div>
          ))}
        </div>

        <div className="button-row">
          <button type="submit" className="primary" disabled={busy}>
            {busy ? <span className="spinner" /> : editing ? 'Save changes' : 'Create broker'}
          </button>
          <button type="button" onClick={() => navigate(-1)}>
            Cancel
          </button>
        </div>
      </form>
    </>
  )
}
