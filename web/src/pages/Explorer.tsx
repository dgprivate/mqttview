import { useCallback, useEffect, useRef, useState } from 'react'
import { Link, useParams } from 'react-router-dom'
import { api, looksBinary, payloadToText } from '../api/client'
import type { Connection, TreeNode } from '../api/types'
import { useAuth } from '../auth/AuthContext'
import { Alert, formatBytes, formatTime, Spinner, StatusBadge, prettyPayload } from '../components/common'
import { TopicTree } from '../components/TopicTree'
import { useConnectionStatus, useLiveMessages } from '../ws/socket'

/**
 * Explorer is the main working surface: the topic tree on the left, the live
 * message stream and the selected topic's payload on the right, and a publish
 * form underneath. On a phone the columns stack.
 */
export function Explorer() {
  const { id = '' } = useParams<{ id: string }>()
  const { can } = useAuth()

  const [connection, setConnection] = useState<Connection | null>(null)
  const [error, setError] = useState('')
  const [selected, setSelected] = useState('')
  const [selectedNode, setSelectedNode] = useState<TreeNode | null>(null)
  const [filter, setFilter] = useState('')
  const [appliedFilter, setAppliedFilter] = useState('')
  const [treeVersion, setTreeVersion] = useState(0)

  const liveStatus = useConnectionStatus()
  const { messages, paused, setPaused, clear } = useLiveMessages(id, appliedFilter)

  const load = useCallback(async () => {
    try {
      setConnection(await api.connection(id))
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Could not load the connection')
    }
  }, [id])

  useEffect(() => {
    void load()
  }, [load])

  // Refresh the tree periodically rather than on every message: a busy broker
  // would otherwise re-render the tree hundreds of times a second.
  const messageCount = messages.length
  const lastRefresh = useRef(0)
  useEffect(() => {
    const now = Date.now()
    if (now - lastRefresh.current < 1500) return
    lastRefresh.current = now
    setTreeVersion((v) => v + 1)
  }, [messageCount])

  useEffect(() => {
    if (!selected) {
      setSelectedNode(null)
      return
    }
    api
      .topic(id, selected)
      .then(setSelectedNode)
      .catch(() => setSelectedNode(null))
  }, [id, selected, treeVersion])

  if (!connection) return error ? <Alert kind="error">{error}</Alert> : <Spinner />

  const status = liveStatus[id] ?? connection.status

  return (
    <>
      <div className="page-head">
        <div>
          <h1>{connection.name}</h1>
          <p className="subtitle">
            <span className="mono">{connection.url}</span> · MQTT {connection.version} ·{' '}
            {connection.topics.toLocaleString()} topics
          </p>
        </div>
        <div className="button-row">
          <StatusBadge status={status} />
          {can('operator') && (
            <button
              onClick={async () => {
                try {
                  if (status.state === 'connected') await api.disconnect(id)
                  else await api.connect(id)
                  await load()
                } catch (err) {
                  setError(err instanceof Error ? err.message : 'Action failed')
                }
              }}
            >
              {status.state === 'connected' ? 'Disconnect' : 'Connect'}
            </button>
          )}
          {can('admin') && (
            <Link className="btn" to={`/connections/${id}/edit`}>
              Edit
            </Link>
          )}
        </div>
      </div>

      <Alert kind="error">{error}</Alert>
      {status.lastError && status.state !== 'connected' && (
        <Alert kind="error">{status.lastError}</Alert>
      )}
      {connection.treeFull && (
        <Alert kind="info">
          The topic tree hit its size limit. New topics are no longer being recorded — narrow the
          subscriptions or clear the view.
        </Alert>
      )}

      <div className="grid explorer">
        <div className="card">
          <div className="card-head">
            <h2>Topics</h2>
            {can('operator') && (
              <button
                className="small"
                onClick={async () => {
                  await api.clearState(id)
                  clear()
                  setSelected('')
                  setTreeVersion((v) => v + 1)
                  await load()
                }}
              >
                Clear
              </button>
            )}
          </div>
          <TopicTree
            connectionId={id}
            selected={selected}
            onSelect={setSelected}
            refreshKey={treeVersion}
          />
        </div>

        <div>
          {selectedNode?.value && (
            <div className="card">
              <div className="card-head">
                <h2 style={{ wordBreak: 'break-all' }} className="mono">
                  {selectedNode.value.topic}
                </h2>
                <div className="button-row">
                  {selectedNode.value.retain && <span className="badge warn">retained</span>}
                  <span className="badge">QoS {selectedNode.value.qos}</span>
                  <span className="badge">{formatBytes(selectedNode.value.size)}</span>
                </div>
              </div>
              <p className="subtitle">
                Updated {formatTime(selectedNode.value.updatedAt)} ·{' '}
                {selectedNode.value.count.toLocaleString()} messages
                {looksBinary(selectedNode.value.payload) && ' · binary payload shown as hex'}
              </p>
              <pre className="payload">
                {prettyPayload(payloadToText(selectedNode.value.payload)) || '(empty payload)'}
              </pre>
              {selectedNode.value.truncated && (
                <p className="subtitle">Payload truncated for display.</p>
              )}
            </div>
          )}

          <div className="card">
            <div className="card-head">
              <h2>Live messages</h2>
              <div className="button-row">
                <button className="small" onClick={() => setPaused(!paused)}>
                  {paused ? 'Resume' : 'Pause'}
                </button>
                <button className="small" onClick={clear}>
                  Clear
                </button>
              </div>
            </div>

            <div className="field-row two">
              <div className="field">
                <label htmlFor="stream-filter">Stream filter (MQTT topic filter)</label>
                <input
                  id="stream-filter"
                  className="mono"
                  value={filter}
                  onChange={(e) => setFilter(e.target.value)}
                  placeholder="home/+/temperature"
                  onKeyDown={(e) => {
                    if (e.key === 'Enter') setAppliedFilter(filter.trim())
                  }}
                />
              </div>
              <div className="field" style={{ display: 'flex', alignItems: 'flex-end' }}>
                <div className="button-row">
                  <button onClick={() => setAppliedFilter(filter.trim())}>Apply</button>
                  <button
                    onClick={() => {
                      setFilter('')
                      setAppliedFilter('')
                    }}
                  >
                    Reset
                  </button>
                </div>
              </div>
            </div>

            {paused && <Alert kind="info">Stream paused — new messages are not being shown.</Alert>}

            <div className="stream">
              {messages.length === 0 ? (
                <p className="subtitle">
                  {status.state === 'connected'
                    ? 'Waiting for messages…'
                    : 'Connect to the broker to see live messages.'}
                </p>
              ) : (
                messages.map((m) => (
                  <div className="msg" key={`${m.connectionId}-${m.seq}`}>
                    <div className="msg-head">
                      <button
                        type="button"
                        className="msg-topic"
                        style={{
                          background: 'none',
                          border: 'none',
                          padding: 0,
                          minHeight: 'auto',
                          cursor: 'pointer',
                        }}
                        onClick={() => setSelected(m.topic)}
                      >
                        {m.topic}
                      </button>
                      <span className="msg-time">
                        {m.retain ? '↺ ' : ''}
                        Q{m.qos} · {formatTime(m.receivedAt)}
                      </span>
                    </div>
                    <pre className="payload">
                      {prettyPayload(payloadToText(m.payload)) || '(empty)'}
                    </pre>
                  </div>
                ))
              )}
            </div>
          </div>

          {can('operator') && <PublishPanel connectionId={id} topic={selected} />}
        </div>
      </div>
    </>
  )
}

function PublishPanel({ connectionId, topic }: { connectionId: string; topic: string }) {
  const [form, setForm] = useState({ topic: '', payload: '', qos: 0, retain: false })
  const [status, setStatus] = useState<{ kind: 'error' | 'ok'; text: string } | null>(null)
  const [busy, setBusy] = useState(false)

  // Selecting a topic in the tree pre-fills the publish form, which is what a
  // user reaching for "publish" almost always wants.
  useEffect(() => {
    if (topic) setForm((prev) => ({ ...prev, topic }))
  }, [topic])

  async function submit(event: React.FormEvent) {
    event.preventDefault()
    setStatus(null)
    setBusy(true)
    try {
      await api.publish(connectionId, {
        topic: form.topic,
        payload: form.payload,
        qos: form.qos,
        retain: form.retain,
      })
      setStatus({ kind: 'ok', text: `Published to ${form.topic}` })
    } catch (err) {
      setStatus({ kind: 'error', text: err instanceof Error ? err.message : 'Publish failed' })
    } finally {
      setBusy(false)
    }
  }

  return (
    <div className="card">
      <h2>Publish</h2>
      {status && <Alert kind={status.kind}>{status.text}</Alert>}

      <form onSubmit={submit}>
        <div className="field">
          <label htmlFor="pub-topic">Topic</label>
          <input
            id="pub-topic"
            className="mono"
            value={form.topic}
            onChange={(e) => setForm({ ...form, topic: e.target.value })}
            placeholder="home/kitchen/light/set"
            required
          />
        </div>

        <div className="field">
          <label htmlFor="pub-payload">Payload</label>
          <textarea
            id="pub-payload"
            value={form.payload}
            onChange={(e) => setForm({ ...form, payload: e.target.value })}
            placeholder='{"state":"ON"}'
          />
        </div>

        <div className="field-row two">
          <div className="field">
            <label htmlFor="pub-qos">QoS</label>
            <select
              id="pub-qos"
              value={form.qos}
              onChange={(e) => setForm({ ...form, qos: Number(e.target.value) })}
            >
              <option value={0}>0 — at most once</option>
              <option value={1}>1 — at least once</option>
              <option value={2}>2 — exactly once</option>
            </select>
          </div>
          <div className="checkbox" style={{ alignSelf: 'flex-end' }}>
            <input
              id="pub-retain"
              type="checkbox"
              checked={form.retain}
              onChange={(e) => setForm({ ...form, retain: e.target.checked })}
            />
            <label htmlFor="pub-retain">Retain</label>
          </div>
        </div>

        <button type="submit" className="primary" disabled={busy}>
          {busy ? <span className="spinner" /> : 'Publish'}
        </button>
      </form>
    </div>
  )
}
