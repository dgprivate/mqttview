import { useCallback, useEffect, useState } from 'react'
import { api } from '../api/client'
import type { PublishRecord, SavedMessage } from '../api/types'
import { Alert, formatTime } from './common'

/**
 * MessageLibrary is the two halves of "send that again": what was sent, and
 * what somebody chose to keep.
 *
 * Republishing goes through the server by id rather than filling the publish
 * form, so the bytes that go out are the bytes that were stored. A binary
 * payload round-tripped through a text field is a different message.
 */
export function MessageLibrary({
  connectionId,
  canOperate,
  onFill,
}: {
  connectionId: string
  canOperate: boolean
  onFill: (topic: string, payload: string) => void
}) {
  const [tab, setTab] = useState<'history' | 'saved'>('history')

  return (
    <div className="card">
      <div className="card-head">
        <h2>Messages</h2>
        <div className="tabs" role="tablist">
          <button
            role="tab"
            aria-selected={tab === 'history'}
            className={tab === 'history' ? 'tab active' : 'tab'}
            onClick={() => setTab('history')}
          >
            Sent
          </button>
          <button
            role="tab"
            aria-selected={tab === 'saved'}
            className={tab === 'saved' ? 'tab active' : 'tab'}
            onClick={() => setTab('saved')}
          >
            Saved
          </button>
        </div>
      </div>

      {tab === 'history' ? (
        <PublishHistory connectionId={connectionId} canOperate={canOperate} onFill={onFill} />
      ) : (
        <SavedMessages connectionId={connectionId} canOperate={canOperate} onFill={onFill} />
      )}
    </div>
  )
}

function PublishHistory({
  connectionId,
  canOperate,
  onFill,
}: {
  connectionId: string
  canOperate: boolean
  onFill: (topic: string, payload: string) => void
}) {
  const [records, setRecords] = useState<PublishRecord[]>([])
  const [query, setQuery] = useState('')
  const [status, setStatus] = useState<{ kind: 'error' | 'ok'; text: string } | null>(null)
  const [busy, setBusy] = useState(false)

  const load = useCallback(
    async (q: string) => {
      try {
        setRecords(await api.publishHistory(connectionId, q, 100))
      } catch (err) {
        setStatus({ kind: 'error', text: err instanceof Error ? err.message : 'Could not read the history' })
      }
    },
    [connectionId],
  )

  useEffect(() => {
    void load('')
  }, [load])

  async function republish(record: PublishRecord) {
    setBusy(true)
    setStatus(null)
    try {
      await api.republish(connectionId, record.id)
      setStatus({ kind: 'ok', text: `Sent again to ${record.topic}` })
      await load(query)
    } catch (err) {
      setStatus({ kind: 'error', text: err instanceof Error ? err.message : 'Could not send it again' })
    } finally {
      setBusy(false)
    }
  }

  async function clearAll() {
    if (!window.confirm('Forget everything this connection has sent?')) return
    try {
      await api.clearPublishHistory(connectionId)
      await load(query)
    } catch (err) {
      setStatus({ kind: 'error', text: err instanceof Error ? err.message : 'Could not clear the history' })
    }
  }

  return (
    <>
      <div className="field-row two">
        <div className="field">
          <label htmlFor="history-search">Search topic or payload</label>
          <input
            id="history-search"
            value={query}
            onChange={(e) => setQuery(e.target.value)}
            onKeyDown={(e) => {
              if (e.key === 'Enter') void load(query.trim())
            }}
            placeholder="kitchen"
          />
        </div>
        <div className="field" style={{ display: 'flex', alignItems: 'flex-end' }}>
          <div className="button-row">
            <button onClick={() => void load(query.trim())}>Search</button>
            {canOperate && records.length > 0 && (
              <button className="small danger" onClick={clearAll}>
                Clear
              </button>
            )}
          </div>
        </div>
      </div>

      {status && <Alert kind={status.kind}>{status.text}</Alert>}

      {records.length === 0 ? (
        <p className="subtitle">Nothing has been published on this connection yet.</p>
      ) : (
        <div className="stream">
          {records.map((r) => (
            <div className="msg" key={r.id}>
              <div className="msg-head">
                <span className="msg-topic mono">{r.topic}</span>
                <span className="msg-time">
                  {r.retain ? '↺ ' : ''}Q{r.qos} · {formatTime(r.publishedAt)}
                  {r.username && ` · ${r.username}`}
                </span>
              </div>
              <pre className="payload">
                {r.payloadBase64 ? `(binary, ${r.payload.length} base64 characters)` : r.payload || '(empty)'}
              </pre>
              <div className="button-row">
                {canOperate && (
                  <button className="small" onClick={() => republish(r)} disabled={busy}>
                    Send again
                  </button>
                )}
                {!r.payloadBase64 && (
                  <button className="small" onClick={() => onFill(r.topic, r.payload)}>
                    Edit and send
                  </button>
                )}
              </div>
            </div>
          ))}
        </div>
      )}
    </>
  )
}

function SavedMessages({
  connectionId,
  canOperate,
  onFill,
}: {
  connectionId: string
  canOperate: boolean
  onFill: (topic: string, payload: string) => void
}) {
  const [messages, setMessages] = useState<SavedMessage[]>([])
  const [status, setStatus] = useState<{ kind: 'error' | 'ok'; text: string } | null>(null)
  const [adding, setAdding] = useState(false)
  const [form, setForm] = useState({ name: '', folder: '', topic: '', payload: '', qos: 0, retain: false, global: false })

  const load = useCallback(async () => {
    try {
      setMessages(await api.savedMessages(connectionId))
    } catch (err) {
      setStatus({ kind: 'error', text: err instanceof Error ? err.message : 'Could not read the collection' })
    }
  }, [connectionId])

  useEffect(() => {
    void load()
  }, [load])

  async function save(event: React.FormEvent) {
    event.preventDefault()
    setStatus(null)
    try {
      await api.createSaved(connectionId, { ...form, sortOrder: 0 })
      setForm({ name: '', folder: '', topic: '', payload: '', qos: 0, retain: false, global: false })
      setAdding(false)
      await load()
    } catch (err) {
      setStatus({ kind: 'error', text: err instanceof Error ? err.message : 'Could not save it' })
    }
  }

  async function send(message: SavedMessage) {
    setStatus(null)
    try {
      await api.publishSaved(connectionId, message.id)
      setStatus({ kind: 'ok', text: `Sent ${message.name}` })
    } catch (err) {
      setStatus({ kind: 'error', text: err instanceof Error ? err.message : 'Could not send it' })
    }
  }

  async function remove(message: SavedMessage) {
    if (!window.confirm(`Delete "${message.name}"?`)) return
    try {
      await api.deleteSaved(connectionId, message.id)
      await load()
    } catch (err) {
      setStatus({ kind: 'error', text: err instanceof Error ? err.message : 'Could not delete it' })
    }
  }

  // Grouped by folder, with the unfiled ones first: a collection with three
  // entries should not make somebody invent a folder for them.
  const folders = new Map<string, SavedMessage[]>()
  for (const m of messages) {
    const key = m.folder || ''
    folders.set(key, [...(folders.get(key) ?? []), m])
  }
  const ordered = [...folders.entries()].sort(([a], [b]) => (a === '' ? -1 : b === '' ? 1 : a.localeCompare(b)))

  return (
    <>
      {status && <Alert kind={status.kind}>{status.text}</Alert>}

      {canOperate && (
        <div className="button-row">
          <button className="small" onClick={() => setAdding(!adding)}>
            {adding ? 'Cancel' : 'Save a message'}
          </button>
        </div>
      )}

      {adding && (
        <form onSubmit={save} className="saved-form">
          <div className="field-row two">
            <div className="field">
              <label htmlFor="saved-name">Name</label>
              <input
                id="saved-name"
                value={form.name}
                onChange={(e) => setForm({ ...form, name: e.target.value })}
                placeholder="kitchen light on"
                required
              />
            </div>
            <div className="field">
              <label htmlFor="saved-folder">Folder (optional)</label>
              <input
                id="saved-folder"
                value={form.folder}
                onChange={(e) => setForm({ ...form, folder: e.target.value })}
                placeholder="lights"
              />
            </div>
          </div>
          <div className="field">
            <label htmlFor="saved-topic">Topic</label>
            <input
              id="saved-topic"
              className="mono"
              value={form.topic}
              onChange={(e) => setForm({ ...form, topic: e.target.value })}
              placeholder="home/kitchen/light/set"
              required
            />
          </div>
          <div className="field">
            <label htmlFor="saved-payload">Payload</label>
            <textarea
              id="saved-payload"
              className="mono"
              rows={3}
              value={form.payload}
              onChange={(e) => setForm({ ...form, payload: e.target.value })}
            />
          </div>
          <div className="field-row three">
            <div className="field">
              <label htmlFor="saved-qos">QoS</label>
              <select
                id="saved-qos"
                value={form.qos}
                onChange={(e) => setForm({ ...form, qos: Number(e.target.value) })}
              >
                <option value={0}>0</option>
                <option value={1}>1</option>
                <option value={2}>2</option>
              </select>
            </div>
            <label className="checkbox">
              <input
                type="checkbox"
                checked={form.retain}
                onChange={(e) => setForm({ ...form, retain: e.target.checked })}
              />
              Retain
            </label>
            <label className="checkbox">
              <input
                type="checkbox"
                checked={form.global}
                onChange={(e) => setForm({ ...form, global: e.target.checked })}
              />
              Available on every broker
            </label>
          </div>
          <div className="button-row">
            <button type="submit">Save</button>
          </div>
        </form>
      )}

      {messages.length === 0 ? (
        <p className="subtitle">
          Nothing saved yet. A saved message is one worth sending more than once — a light on, a
          device reset, a test payload.
        </p>
      ) : (
        ordered.map(([folder, items]) => (
          <div key={folder || '(unfiled)'} className="saved-folder">
            {folder && <h3 className="saved-folder-name">{folder}</h3>}
            {items.map((m) => (
              <div className="saved-item" key={m.id}>
                <div className="saved-item-head">
                  <strong>{m.name}</strong>
                  <span className="subtitle mono">{m.topic}</span>
                </div>
                <div className="button-row">
                  {canOperate && (
                    <button className="small" onClick={() => send(m)}>
                      Send
                    </button>
                  )}
                  {!m.payloadBase64 && (
                    <button className="small" onClick={() => onFill(m.topic, m.payload)}>
                      Edit and send
                    </button>
                  )}
                  {canOperate && (
                    <button className="small danger" onClick={() => remove(m)}>
                      Delete
                    </button>
                  )}
                  {!m.connectionId && <span className="badge">every broker</span>}
                </div>
              </div>
            ))}
          </div>
        ))
      )}
    </>
  )
}
