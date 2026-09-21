import { useCallback, useEffect, useState } from 'react'
import { Link, useParams } from 'react-router-dom'
import { api } from '../api/client'
import type { Connection, RecordingsResponse } from '../api/types'
import { useAuth } from '../auth/AuthContext'
import { Alert, formatBytes, formatTime, prettyPayload, Spinner } from '../components/common'
import { toInput } from './BrokerStatus'

/**
 * Recordings reads back what was written to disk, for the questions the
 * in-memory history is too small to answer: what did this device do overnight,
 * and what changed between Tuesday and Thursday.
 */
export function Recordings() {
  const { id = '' } = useParams<{ id: string }>()
  const { can } = useAuth()

  const [data, setData] = useState<RecordingsResponse | null>(null)
  const [connection, setConnection] = useState<Connection | null>(null)
  const [topic, setTopic] = useState('')
  const [since, setSince] = useState('')
  const [until, setUntil] = useState('')
  const [error, setError] = useState('')
  const [busy, setBusy] = useState(false)

  const load = useCallback(async () => {
    try {
      const [recordings, conn] = await Promise.all([
        api.recordings(id, {
          topic: topic.trim() || undefined,
          since: since ? new Date(since).toISOString() : undefined,
          until: until ? new Date(until).toISOString() : undefined,
          limit: 500,
        }),
        api.connection(id),
      ])
      setData(recordings)
      setConnection(conn)
      setError('')
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Could not read the recordings')
    }
  }, [id, topic, since, until])

  useEffect(() => {
    void load()
    // Only on mount and when the query is submitted; the fields are read then.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [id])

  async function toggleRecording(enabled: boolean) {
    if (!connection) return
    setBusy(true)
    try {
      await api.updateConnection(id, { ...toInput(connection), recordToDisk: enabled })
      await load()
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Could not change the setting')
    } finally {
      setBusy(false)
    }
  }

  async function deleteAll() {
    const ok = window.confirm(
      `Delete every recorded message for ${connection?.name ?? 'this connection'}?\n\n` +
        'This cannot be undone.',
    )
    if (!ok) return

    setBusy(true)
    try {
      await api.deleteRecordings(id)
      await load()
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Could not delete the recordings')
    } finally {
      setBusy(false)
    }
  }

  if (!data || !connection) {
    return error ? <Alert kind="error">{error}</Alert> : <Spinner label="Reading the recordings…" />
  }

  return (
    <>
      <div className="page-head">
        <div>
          <h1>Recordings</h1>
          <p className="subtitle">{connection.name}</p>
        </div>
        <div className="button-row">
          <Link className="btn small" to={`/connections/${id}`}>
            Explorer
          </Link>
        </div>
      </div>

      {error && <Alert kind="error">{error}</Alert>}

      {!data.recording && (
        <Alert kind="info">
          Recording is off for this connection. It is the one setting that makes the database grow
          with broker traffic rather than with configuration, so it stays off until it is asked for.
          {can('admin') && (
            <>
              {' '}
              <button className="small" onClick={() => toggleRecording(true)} disabled={busy}>
                {busy ? 'Turning on…' : 'Start recording'}
              </button>
            </>
          )}
        </Alert>
      )}

      {data.stats && (
        <div className="tiles">
          <div className="tile">
            <span className="tile-label">Messages on disk</span>
            <span className="tile-value">{data.stats.rows.toLocaleString()}</span>
          </div>
          <div className="tile">
            <span className="tile-label">Payload stored</span>
            <span className="tile-value">{formatBytes(data.stats.payloadBytes)}</span>
          </div>
          <div className="tile">
            <span className="tile-label">Oldest</span>
            <span className="tile-value">
              {data.stats.oldest ? formatTime(data.stats.oldest) : <span className="subtitle">none</span>}
            </span>
          </div>
          {data.recorder && data.recorder.dropped > 0 && (
            <div className="tile warn">
              <span className="tile-label">Dropped</span>
              <span className="tile-value">{data.recorder.dropped.toLocaleString()}</span>
            </div>
          )}
        </div>
      )}

      {data.recorder && data.recorder.dropped > 0 && (
        <Alert kind="error">
          {data.recorder.dropped.toLocaleString()} messages could not be written and are missing
          from this recording. That happens when messages arrive faster than the disk accepts them.
          Treat gaps here as unknown rather than as nothing having happened.
        </Alert>
      )}

      <div className="card">
        <h2>Find</h2>
        <div className="field-row three">
          <div className="field">
            <label htmlFor="rec-topic">Topic (exact)</label>
            <input
              id="rec-topic"
              className="mono"
              value={topic}
              onChange={(e) => setTopic(e.target.value)}
              placeholder="home/kitchen/temperature"
            />
          </div>
          <div className="field">
            <label htmlFor="rec-since">From</label>
            <input
              id="rec-since"
              type="datetime-local"
              value={since}
              onChange={(e) => setSince(e.target.value)}
            />
          </div>
          <div className="field">
            <label htmlFor="rec-until">To</label>
            <input
              id="rec-until"
              type="datetime-local"
              value={until}
              onChange={(e) => setUntil(e.target.value)}
            />
          </div>
        </div>
        <div className="button-row">
          <button onClick={() => void load()}>Search</button>
          <button
            className="small"
            onClick={() => {
              setTopic('')
              setSince('')
              setUntil('')
            }}
          >
            Reset
          </button>
          {can('operator') && data.stats && data.stats.rows > 0 && (
            <button className="small danger" onClick={deleteAll} disabled={busy}>
              Delete all
            </button>
          )}
          {can('admin') && data.recording && (
            <button className="small" onClick={() => toggleRecording(false)} disabled={busy}>
              Stop recording
            </button>
          )}
        </div>
      </div>

      <div className="card">
        <h2>Messages</h2>
        {data.messages.length === 0 ? (
          <p className="subtitle">
            {data.recording
              ? 'Nothing recorded for this search yet.'
              : 'Nothing has been recorded for this connection.'}
          </p>
        ) : (
          <div className="stream">
            {data.messages.map((m) => (
              <div className="msg" key={m.id}>
                <div className="msg-head">
                  <span className="msg-topic mono">{m.topic}</span>
                  <span className="msg-time">
                    {m.retain ? '↺ ' : ''}Q{m.qos} · {formatTime(m.receivedAt)}
                  </span>
                </div>
                <pre className="payload">
                  {m.payloadBase64
                    ? `(binary, ${m.payload.length} base64 characters)`
                    : prettyPayload(m.payload) || '(empty)'}
                </pre>
              </div>
            ))}
          </div>
        )}
      </div>
    </>
  )
}
