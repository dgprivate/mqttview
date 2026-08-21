import { useCallback, useEffect, useRef, useState } from 'react'
import { Link } from 'react-router-dom'
import { api } from '../api/client'
import type { Connection, PlcEdge, PlcPoint, PlcState, PlcStatus } from '../api/types'
import { Alert, Empty, formatRelative, formatTime, Spinner } from '../components/common'
import { useFrames } from '../ws/socket'

type Tab = 'discovery' | 'io' | 'lights' | 'power'

const TABS: { id: Tab; label: string }[] = [
  { id: 'discovery', label: 'Discovery' },
  { id: 'io', label: 'I/O' },
  { id: 'lights', label: 'Lights' },
  { id: 'power', label: 'Power' },
]

/** maxLog is how many transitions the discovery view keeps on screen. */
const maxLog = 200

/**
 * BeckhoffPlc renders a PLC's I/O by room and name, and gives a live view of
 * which digital signals are moving.
 *
 * The discovery tab is the reason the page exists: press a wall switch and the
 * point that moved is named on screen, which is what a PLC programmer needs in
 * order to write logic against it.
 */
export function BeckhoffPlc() {
  const [tab, setTab] = useState<Tab>('discovery')
  const [status, setStatus] = useState<PlcStatus | null>(null)
  const [state, setState] = useState<PlcState | null>(null)
  const [connections, setConnections] = useState<Connection[]>([])
  const [connectionId, setConnectionId] = useState('')
  const [log, setLog] = useState<PlcEdge[]>([])
  const [error, setError] = useState('')
  const [disabled, setDisabled] = useState(false)

  // The newest sequence number already shown, so a refresh appends rather than
  // reloading the whole journal.
  const seen = useRef(0)

  const loadState = useCallback(async () => {
    try {
      const [s, st] = await Promise.all([api.plcStatus(), api.plcState(connectionId)])
      setStatus(s)
      setState(st)
      setDisabled(false)
      setError('')
    } catch (err) {
      const message = err instanceof Error ? err.message : 'Could not load the PLC'
      if (message.includes('disabled') || message.includes('not enabled')) {
        setDisabled(true)
      } else {
        setError(message)
      }
    }
  }, [connectionId])

  const loadEdges = useCallback(async () => {
    try {
      const page = await api.plcEdges({ connectionId, since: seen.current, limit: maxLog })
      if (page.edges.length === 0) {
        seen.current = page.seq
        return
      }
      seen.current = page.seq
      setLog((prev) => [...page.edges].reverse().concat(prev).slice(0, maxLog))
    } catch {
      // A failed poll is not worth a banner; the next event will retry.
    }
  }, [connectionId])

  useEffect(() => {
    api.connections().then(setConnections).catch(() => undefined)
  }, [])

  useEffect(() => {
    // Switching broker changes what the log means, so start it over.
    seen.current = 0
    setLog([])
    void loadState()
    void loadEdges()
  }, [loadState, loadEdges])

  // The plugin batches its change events to four a second at most, so
  // refetching on each one is cheap.
  useFrames((frame) => {
    if (frame.type === 'event' && frame.event === 'plugin:beckhoff-plc:changed') {
      void loadState()
      void loadEdges()
    }
  })

  if (disabled) {
    return (
      <Empty>
        The Beckhoff PLC plugin is switched off. <Link to="/plugins">Enable it on the plugins page</Link>
      </Empty>
    )
  }
  if (!state || !status) return error ? <Alert kind="error">{error}</Alert> : <Spinner />

  return (
    <>
      <div className="page-head">
        <div>
          <h1>Beckhoff PLC</h1>
          <p className="subtitle">
            {status.points} points, {status.lights} lights under <code>{status.topicPrefix}/</code> — read only
          </p>
        </div>
        {connections.length > 1 && (
          <select value={connectionId} onChange={(e) => setConnectionId(e.target.value)}>
            <option value="">All brokers</option>
            {connections.map((c) => (
              <option key={c.id} value={c.id}>
                {c.name}
              </option>
            ))}
          </select>
        )}
      </div>

      {error && <Alert kind="error">{error}</Alert>}

      <div className="tabs">
        {TABS.map((t) => (
          <button
            key={t.id}
            className={`tab ${tab === t.id ? 'active' : ''}`}
            onClick={() => setTab(t.id)}
            aria-current={tab === t.id}
          >
            {t.label}
          </button>
        ))}
      </div>

      {tab === 'discovery' && <Discovery log={log} onClear={() => setLog([])} />}
      {tab === 'io' && <PointsView points={state.points} />}
      {tab === 'lights' && <LightsView state={state} />}
      {tab === 'power' && <PowerView state={state} />}
    </>
  )
}

/** describe names a point as helpfully as the metadata allows. */
function describe(e: { label?: string; name: string; address: number }): string {
  return e.label || e.name || `address ${e.address}`
}

function Discovery({ log, onClear }: { log: PlcEdge[]; onClear: () => void }) {
  const latest = log[0]

  return (
    <>
      <div className={`plc-latest ${latest ? (latest.to ? 'on' : 'off') : ''}`}>
        {latest ? (
          <>
            <span className="plc-latest-label">{describe(latest)}</span>
            <span className="plc-latest-meta">
              {latest.name} · address {latest.address} · {latest.kind}
              {latest.location ? ` · ${latest.location}` : ''}
            </span>
            <span className="plc-latest-transition">
              {latest.from ? 'on' : 'off'} → {latest.to ? 'on' : 'off'} · {formatTime(latest.at)}
            </span>
          </>
        ) : (
          <>
            <span className="plc-latest-label">Listening…</span>
            <span className="plc-latest-meta">
              Press a button or trigger a sensor. The point that moves is named here.
            </span>
          </>
        )}
      </div>

      <div className="card">
        <div className="card-head">
          <h2>Signal log</h2>
          <button className="btn" onClick={onClear} disabled={log.length === 0}>
            Clear
          </button>
        </div>
        {log.length === 0 ? (
          <Empty>
            Nothing has changed yet. Only real transitions are logged, so the retained values that arrive when a
            broker connects are not listed here.
          </Empty>
        ) : (
          <div className="table-wrap">
            <table>
              <thead>
                <tr>
                  <th>Time</th>
                  <th>Point</th>
                  <th>Address</th>
                  <th>Location</th>
                  <th>Change</th>
                </tr>
              </thead>
              <tbody>
                {log.map((e) => (
                  <tr key={e.seq}>
                    <td data-label="Time" className="mono">
                      {formatTime(e.at)}
                    </td>
                    <td data-label="Point">
                      {describe(e)}
                      <small className="mono"> {e.name}</small>
                    </td>
                    <td data-label="Address" className="mono">
                      {e.address}
                    </td>
                    <td data-label="Location">{e.location || '—'}</td>
                    <td data-label="Change">
                      <span className={`badge ${e.to ? 'ok' : ''}`}>
                        {e.from ? 'on' : 'off'} → {e.to ? 'on' : 'off'}
                      </span>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </div>
    </>
  )
}

function PointsView({ points }: { points: PlcPoint[] }) {
  const [search, setSearch] = useState('')
  const [kind, setKind] = useState('')

  const needle = search.trim().toLowerCase()
  const visible = points.filter((p) => {
    if (kind && p.kind !== kind) return false
    if (!needle) return true
    return [p.name, p.label, p.location, p.sensorType, String(p.address)]
      .filter(Boolean)
      .some((f) => String(f).toLowerCase().includes(needle))
  })

  return (
    <div className="card">
      <div className="card-head">
        <h2>
          Points <small>{visible.length} of {points.length}</small>
        </h2>
      </div>
      <div className="field-row">
        <input
          type="search"
          placeholder="Search name, label or location"
          value={search}
          onChange={(e) => setSearch(e.target.value)}
        />
        <select value={kind} onChange={(e) => setKind(e.target.value)}>
          <option value="">All kinds</option>
          <option value="input">Inputs</option>
          <option value="output">Outputs</option>
          <option value="temperature">Temperatures</option>
        </select>
      </div>

      {visible.length === 0 ? (
        <Empty>Nothing matches.</Empty>
      ) : (
        <div className="table-wrap">
          <table>
            <thead>
              <tr>
                <th>Point</th>
                <th>Kind</th>
                <th>Address</th>
                <th>Location</th>
                <th>Value</th>
                <th>Updated</th>
              </tr>
            </thead>
            <tbody>
              {visible.map((p) => (
                <tr key={p.topic}>
                  <td data-label="Point">
                    {describe(p)}
                    <small className="mono"> {p.name}</small>
                  </td>
                  <td data-label="Kind">{p.kind}</td>
                  <td data-label="Address" className="mono">
                    {p.address}
                  </td>
                  <td data-label="Location">{p.location || '—'}</td>
                  <td data-label="Value">
                    {p.number !== undefined ? (
                      <span className="mono">{p.number.toFixed(2)}</span>
                    ) : (
                      <span className={`badge ${p.bool ? 'ok' : ''}`}>{p.bool ? 'on' : 'off'}</span>
                    )}
                  </td>
                  <td data-label="Updated">{formatRelative(p.updatedAt)}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </div>
  )
}

function LightsView({ state }: { state: PlcState }) {
  const { lights, summary } = state
  const [onlyErrors, setOnlyErrors] = useState(false)
  const visible = onlyErrors ? lights.filter((l) => l.error) : lights

  return (
    <div className="card">
      <div className="card-head">
        <h2>
          DALI lights <small>{summary.lightsOn} on, {summary.lightsWithError} reporting an error</small>
        </h2>
        <label className="checkbox">
          <input type="checkbox" checked={onlyErrors} onChange={(e) => setOnlyErrors(e.target.checked)} />
          <span>Only errors</span>
        </label>
      </div>

      {visible.length === 0 ? (
        <Empty>No lights to show.</Empty>
      ) : (
        <div className="plc-lights">
          {visible.map((l) => {
            const percent = l.actualLevel > 0 ? Math.round((l.actualLevel / 254) * 100) : 0
            return (
              <div key={l.address} className={`plc-light ${l.error ? 'bad' : ''}`} title={l.error || ''}>
                <div className="plc-light-head">
                  <strong className="mono">{l.name || l.address}</strong>
                  <span>{percent}%</span>
                </div>
                <div className="plc-bar">
                  <span style={{ width: `${percent}%` }} />
                </div>
                <small>
                  min {l.minLevel} · max {l.maxLevel}
                  {l.error ? ` · ${l.error}` : ''}
                </small>
              </div>
            )
          })}
        </div>
      )}
    </div>
  )
}

function PowerView({ state }: { state: PlcState }) {
  const { electricity, meters, watchdog } = state

  return (
    <>
      {watchdog && (
        <div className="card">
          <div className="card-head">
            <h2>Controller</h2>
            <span className={`badge ${watchdog.alive && watchdog.ready ? 'ok' : 'err'}`}>
              {watchdog.alive ? 'alive' : 'not alive'}
            </span>
          </div>
          <dl className="plc-facts">
            <div>
              <dt>Uptime</dt>
              <dd>{(watchdog.uptimeS / 3600).toFixed(1)} h</dd>
            </div>
            <div>
              <dt>Alarm</dt>
              <dd>
                {watchdog.alarmMode}
                {watchdog.alarmTriggered ? ' · triggered' : ''}
              </dd>
            </div>
            <div>
              <dt>Broker</dt>
              <dd>
                {watchdog.mqttConnected ? 'primary ok' : 'primary down'}
                {watchdog.backupMqttConnected ? ', backup ok' : ''}
              </dd>
            </div>
            {watchdog.streams &&
              Object.entries(watchdog.streams).map(([name, s]) => (
                <div key={name}>
                  <dt>{name} stream</dt>
                  <dd className={s.ageS > 300 ? 'warn' : ''}>
                    {s.count.toLocaleString()} msgs · {s.ageS}s old
                  </dd>
                </div>
              ))}
          </dl>
        </div>
      )}

      {electricity.map((e) => (
        <div className="card" key={e.name}>
          <div className="card-head">
            <h2>Electricity — {e.name}</h2>
            {e.alarmActive && <span className="badge err">alarm</span>}
          </div>
          <div className="table-wrap">
            <table>
              <thead>
                <tr>
                  <th>Phase</th>
                  <th>Voltage</th>
                  <th>Current</th>
                </tr>
              </thead>
              <tbody>
                {e.phases.map((p, i) => (
                  <tr key={i}>
                    <td data-label="Phase">L{i + 1}</td>
                    <td data-label="Voltage" className="mono">
                      {p.voltage.toFixed(1)} V
                    </td>
                    <td data-label="Current" className="mono">
                      {p.current.toFixed(2)} A
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
          <dl className="plc-facts">
            <div>
              <dt>Frequency</dt>
              <dd className="mono">{e.frequency.toFixed(3)} Hz</dd>
            </div>
            <div>
              <dt>Voltage imbalance</dt>
              <dd className="mono">{e.voltageImbalance.toFixed(2)} %</dd>
            </div>
            <div>
              <dt>Current imbalance</dt>
              <dd className={`mono ${e.currentImbalance > 50 ? 'warn' : ''}`}>{e.currentImbalance.toFixed(1)} %</dd>
            </div>
          </dl>
          {e.activeAlarms && e.activeAlarms.length > 0 && (
            <Alert kind="error">{e.activeAlarms.join(', ')}</Alert>
          )}
        </div>
      ))}

      {meters.map((m) => (
        <div className="card" key={m.name}>
          <div className="card-head">
            <h2>{m.name}</h2>
            <span className={`badge ${m.available ? 'ok' : 'err'}`}>{m.available ? 'online' : 'offline'}</span>
          </div>
          <dl className="plc-facts">
            {Object.entries(m.readings ?? {}).map(([k, v]) => (
              <div key={k}>
                <dt>{k.replace(/_/g, ' ')}</dt>
                <dd className="mono">{v}</dd>
              </div>
            ))}
          </dl>
        </div>
      ))}

      {electricity.length === 0 && meters.length === 0 && !watchdog && (
        <Empty>No power or metering data has arrived yet.</Empty>
      )}
    </>
  )
}
