import { useCallback, useEffect, useRef, useState } from 'react'
import { api } from '../api/client'
import type { AuditEntry, LogRecord } from '../api/types'
import { Alert, formatTime, Spinner } from '../components/common'

/**
 * Logs is the server's own log, and the record of what was done to the world
 * outside it.
 *
 * It exists for the installation nobody can get a terminal on — which is every
 * Home Assistant add-on. Both views are admin only: log lines name accounts,
 * topics and broker hostnames.
 */
export function Logs() {
  const [tab, setTab] = useState<'log' | 'audit'>('log')

  return (
    <>
      <div className="page-head">
        <div>
          <h1>Logs</h1>
          <p className="subtitle">What the server logged, and what people did with it</p>
        </div>
      </div>

      <div className="tabs" role="tablist">
        <button
          role="tab"
          aria-selected={tab === 'log'}
          className={tab === 'log' ? 'tab active' : 'tab'}
          onClick={() => setTab('log')}
        >
          Server log
        </button>
        <button
          role="tab"
          aria-selected={tab === 'audit'}
          className={tab === 'audit' ? 'tab active' : 'tab'}
          onClick={() => setTab('audit')}
        >
          Actions
        </button>
      </div>

      {tab === 'log' ? <ServerLog /> : <AuditLog />}
    </>
  )
}

function ServerLog() {
  const [records, setRecords] = useState<LogRecord[]>([])
  const [level, setLevel] = useState('')
  const [follow, setFollow] = useState(true)
  const [error, setError] = useState('')
  const [loaded, setLoaded] = useState(false)
  const seen = useRef(0)
  const bottom = useRef<HTMLDivElement | null>(null)

  // Changing the level re-reads from the beginning: the records below it were
  // never sent, so filtering forward would show an empty view until something
  // new happened at that level.
  useEffect(() => {
    seen.current = 0
    setRecords([])
  }, [level])

  const polling = useRef(false)

  const poll = useCallback(async () => {
    // One read at a time. The first render starts a read and the interval
    // starts another before it returns; both begin from the same anchor, both
    // receive the same records, and the view shows every line twice.
    if (polling.current) return
    polling.current = true
    try {
      const page = await api.logs(level, seen.current, 500)
      seen.current = page.seq
      if (page.records.length > 0) {
        setRecords((prev) => {
          // Belt and braces: a level change resets the anchor, and a read
          // already in flight against the old one can still land afterwards.
          const known = new Set(prev.map((r) => r.seq))
          const fresh = page.records.filter((r) => !known.has(r.seq))
          // Bounded in the browser as well as on the server: a page left open
          // for a day should not accumulate without limit.
          return fresh.length === 0 ? prev : [...prev, ...fresh].slice(-2000)
        })
      }
      setError('')
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Could not read the log')
    } finally {
      polling.current = false
      setLoaded(true)
    }
  }, [level])

  useEffect(() => {
    void poll()
    const timer = setInterval(() => void poll(), 2000)
    return () => clearInterval(timer)
  }, [poll])

  useEffect(() => {
    if (follow) bottom.current?.scrollIntoView({ block: 'end' })
  }, [records, follow])

  if (!loaded) return <Spinner label="Reading the log…" />

  return (
    <div className="card">
      <div className="card-head">
        <div className="field" style={{ marginBottom: 0 }}>
          <label htmlFor="log-level">Level</label>
          <select id="log-level" value={level} onChange={(e) => setLevel(e.target.value)}>
            <option value="">everything collected</option>
            <option value="info">info and above</option>
            <option value="warn">warnings and errors</option>
            <option value="error">errors only</option>
          </select>
        </div>
        <div className="button-row">
          <button className="small" onClick={() => setFollow(!follow)}>
            {follow ? 'Stop following' : 'Follow'}
          </button>
          <button
            className="small"
            onClick={() => {
              seen.current = 0
              setRecords([])
            }}
          >
            Clear view
          </button>
        </div>
      </div>

      {error && <Alert kind="error">{error}</Alert>}

      <p className="subtitle">
        Only what the server's configured log level admits reaches this view. To see debug lines,
        start mqttview with <code>-log-level debug</code>.
      </p>

      <div className="log">
        {records.length === 0 ? (
          <p className="subtitle">Nothing logged at this level yet.</p>
        ) : (
          records.map((r) => (
            <div className={`log-line log-${r.level.toLowerCase()}`} key={r.seq}>
              <span className="log-time">{formatTime(r.time)}</span>
              <span className="log-level">{r.level}</span>
              <span className="log-message">{r.message}</span>
              {r.attrs && (
                <span className="log-attrs">
                  {Object.entries(r.attrs).map(([k, v]) => (
                    <span className="log-attr" key={k}>
                      {k}=<strong>{v}</strong>
                    </span>
                  ))}
                </span>
              )}
            </div>
          ))
        )}
        <div ref={bottom} />
      </div>
    </div>
  )
}

function AuditLog() {
  const [entries, setEntries] = useState<AuditEntry[] | null>(null)
  const [error, setError] = useState('')

  useEffect(() => {
    api
      .audit(200)
      .then(setEntries)
      .catch((err) => setError(err instanceof Error ? err.message : 'Could not read the log'))
  }, [])

  if (error) return <Alert kind="error">{error}</Alert>
  if (!entries) return <Spinner label="Reading what was done…" />

  return (
    <div className="card">
      <h2>Actions</h2>
      <p className="subtitle">
        Everything that changed something outside mqttview: clearing a retained message on a broker,
        deleting a recording. Reading and publishing are in the server log above.
      </p>

      {entries.length === 0 ? (
        <p className="subtitle">Nothing has been done yet.</p>
      ) : (
        <table className="responsive">
          <thead>
            <tr>
              <th>When</th>
              <th>Who</th>
              <th>What</th>
              <th>Where</th>
            </tr>
          </thead>
          <tbody>
            {entries.map((e) => (
              <tr key={e.id}>
                <td data-label="When">{formatTime(e.at)}</td>
                <td data-label="Who">{e.username || '(unknown)'}</td>
                <td data-label="What">
                  {e.action}
                  {e.detail && <span className="subtitle"> — {e.detail}</span>}
                </td>
                <td data-label="Where" className="mono">
                  {e.target}
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
    </div>
  )
}
