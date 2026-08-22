import { useCallback, useEffect, useRef, useState } from 'react'
import { Link } from 'react-router-dom'
import { api } from '../api/client'
import type { Connection, PlcCommands, PlcEdge, PlcMapping, PlcPoint, PlcState, PlcStatus } from '../api/types'
import { useAuth } from '../auth/AuthContext'
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
  const { can } = useAuth()
  const [tab, setTab] = useState<Tab>('discovery')
  const [status, setStatus] = useState<PlcStatus | null>(null)
  const [state, setState] = useState<PlcState | null>(null)
  const [commands, setCommands] = useState<PlcCommands | null>(null)
  const [connections, setConnections] = useState<Connection[]>([])
  const [connectionId, setConnectionId] = useState('')
  const [log, setLog] = useState<PlcEdge[]>([])
  const [mappings, setMappings] = useState<PlcMapping[]>([])
  const [naming, setNaming] = useState<{ name: string; existing?: PlcMapping } | null>(null)
  const [error, setError] = useState('')
  const [disabled, setDisabled] = useState(false)

  // The newest sequence number already shown, so a refresh appends rather than
  // reloading the whole journal.
  const seen = useRef(0)

  const loadState = useCallback(async () => {
    try {
      const [s, st, cmds, maps] = await Promise.all([
        api.plcStatus(),
        api.plcState(connectionId),
        api.plcCommands(),
        api.plcMappings(connectionId),
      ])
      setStatus(s)
      setState(st)
      setCommands(cmds)
      setMappings(maps)
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

  const canOperate = can('operator')
  const mappingFor = (name: string) => mappings.find((m) => m.name === name)
  const openNaming = (name: string) => setNaming({ name, existing: mappingFor(name) })

  const saveMapping = async (m: PlcMapping) => {
    try {
      await api.plcSetMapping({ ...m, connectionId: connectionId || undefined })
      setNaming(null)
      await loadState()
      // Names already in the log were captured with the old label, so refresh
      // them too rather than leaving a stale name on screen.
      seen.current = 0
      setLog([])
      await loadEdges()
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Could not save the name')
    }
  }

  const send = async (target: string, command: string, address?: number, params?: string[]) => {
    setError('')
    try {
      await api.plcCommand({ target, command, address, params, connectionId: connectionId || undefined })
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Command failed')
    }
  }

  return (
    <>
      <div className="page-head">
        <div>
          <h1>
            Beckhoff PLC{' '}
            <span className={`badge ${status.readOnly ? '' : 'warn'}`}>
              {status.readOnly ? 'read only' : 'control on'}
            </span>
          </h1>
          <p className="subtitle">
            {status.points} points and {status.lights} lights under <code>{status.topicPrefix}/</code>
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

      {tab === 'discovery' && (
        <Discovery log={log} onClear={() => setLog([])} onName={canOperate ? openNaming : undefined} />
      )}
      {tab === 'io' && <PointsView points={state.points} onName={canOperate ? openNaming : undefined} />}
      {tab === 'lights' && (
        <LightsView state={state} commands={commands} canOperate={canOperate} onSend={send} />
      )}
      {tab === 'power' && <PowerView state={state} />}

      {naming && (
        <NameDialog
          name={naming.name}
          existing={naming.existing}
          onCancel={() => setNaming(null)}
          onSave={saveMapping}
        />
      )}
    </>
  )
}

/**
 * NameDialog gives a point a human name. The PLC's own name is the key and is
 * shown but not editable: it is what ties the description to the point across
 * an address renumbering.
 */
function NameDialog({
  name,
  existing,
  onCancel,
  onSave,
}: {
  name: string
  existing?: PlcMapping
  onCancel: () => void
  onSave: (m: PlcMapping) => void
}) {
  const [label, setLabel] = useState(existing?.label ?? '')
  const [location, setLocation] = useState(existing?.location ?? '')
  const [type, setType] = useState(existing?.type ?? '')
  const [notes, setNotes] = useState(existing?.notes ?? '')

  return (
    <div className="plc-dialog-backdrop" onClick={onCancel}>
      <div className="plc-dialog" onClick={(e) => e.stopPropagation()}>
        <h2>
          Name <span className="mono">{name}</span>
        </h2>
        <p className="subtitle">
          Your name takes precedence over whatever the PLC publishes about this point. Clear every field to remove
          it.
        </p>
        <div className="field">
          <label htmlFor="plc-label">Label</label>
          <input
            id="plc-label"
            value={label}
            autoFocus
            placeholder="Kuhinja - stikalo pri vratih"
            onChange={(e) => setLabel(e.target.value)}
          />
        </div>
        <div className="field-row">
          <div className="field">
            <label htmlFor="plc-location">Location</label>
            <input
              id="plc-location"
              value={location}
              placeholder="kitchen"
              onChange={(e) => setLocation(e.target.value)}
            />
          </div>
          <div className="field">
            <label htmlFor="plc-type">Type</label>
            <input id="plc-type" value={type} placeholder="button" onChange={(e) => setType(e.target.value)} />
          </div>
        </div>
        <div className="field">
          <label htmlFor="plc-notes">Notes</label>
          <textarea
            id="plc-notes"
            rows={3}
            value={notes}
            placeholder="What should happen when this signal fires? The PLC programmer reads this."
            onChange={(e) => setNotes(e.target.value)}
          />
        </div>
        <div className="button-row">
          <button className="btn primary" onClick={() => onSave({ name, label, location, type, notes })}>
            Save
          </button>
          <button className="btn" onClick={onCancel}>
            Cancel
          </button>
        </div>
      </div>
    </div>
  )
}

/** describe names a point as helpfully as the metadata allows. */
function describe(e: { label?: string; name: string; address: number }): string {
  return e.label || e.name || `address ${e.address}`
}

/**
 * technicalName returns the PLC's own name only when it is not already what is
 * being displayed, so an unnamed point reads "DI-1-1" rather than "DI-1-1
 * DI-1-1".
 */
function technicalName(e: { label?: string; name: string }): string | null {
  return e.label && e.label !== e.name ? e.name : null
}

/**
 * daliFault turns a ballast's raw error into something readable. The PLC
 * forwards the DALI stack's own wording, which arrives truncated mid-word
 * ("ParameterAddressIsAShortAddressAndLiesOutsideOfThe"); the original is kept
 * on the tooltip so nothing is hidden.
 */
function daliFault(error?: string): string | null {
  if (!error) return null
  if (error.startsWith('ParameterAddressIsAShortAddress')) return 'Address not answering'
  if (error.toLowerCase() === 'unknown') return 'No reply'
  return error
}

/** Segmented is a compact filter switch, sized for a thumb. */
function Segmented<T extends string>({
  value,
  onChange,
  options,
}: {
  value: T
  onChange: (v: T) => void
  options: { value: T; label: string; count?: number }[]
}) {
  return (
    <div className="seg" role="group">
      {options.map((o) => (
        <button
          key={o.value}
          className={`seg-btn ${value === o.value ? 'active' : ''}`}
          aria-pressed={value === o.value}
          onClick={() => onChange(o.value)}
        >
          {o.label}
          {o.count !== undefined && <span className="seg-count">{o.count}</span>}
        </button>
      ))}
    </div>
  )
}

/** discoveryPageSize keeps the log readable; the rest is a click away. */
const discoveryPageSize = 30

function Discovery({
  log,
  onClear,
  onName,
}: {
  log: PlcEdge[]
  onClear: () => void
  onName?: (name: string) => void
}) {
  const [risingOnly, setRisingOnly] = useState(false)
  const [hideMotion, setHideMotion] = useState(false)
  const [search, setSearch] = useState('')
  const [limit, setLimit] = useState(discoveryPageSize)
  // Pausing freezes a copy rather than stopping the feed: a house full of
  // motion sensors will not hold still while you read what just happened.
  const [frozen, setFrozen] = useState<PlcEdge[] | null>(null)

  const source = frozen ?? log
  const needle = search.trim().toLowerCase()
  const visible = source.filter((e) => {
    if (risingOnly && !e.to) return false
    if (hideMotion && e.sensorType === 'motion') return false
    if (!needle) return true
    return [e.name, e.label, e.location, e.sensorType, String(e.address)]
      .filter(Boolean)
      .some((f) => String(f).toLowerCase().includes(needle))
  })
  const shown = visible.slice(0, limit)
  const latest = visible[0]

  return (
    <>
      <div className={`plc-latest ${latest ? (latest.to ? 'on' : 'off') : ''}`}>
        <div className="plc-latest-body">
          {latest ? (
            <>
              <span className="plc-latest-label">{describe(latest)}</span>
              <span className="plc-latest-meta">
                <span className="mono">{latest.name}</span> · address {latest.address} · {latest.kind}
                {latest.location ? ` · ${latest.location}` : ''}
              </span>
              <span className="plc-latest-transition">
                {latest.from ? 'on' : 'off'} → {latest.to ? 'on' : 'off'} · {formatTime(latest.at)} ·{' '}
                {formatRelative(latest.at)}
              </span>
            </>
          ) : (
            <>
              <span className="plc-latest-label">
                <span className="plc-pulse" /> Listening…
              </span>
              <span className="plc-latest-meta">
                Press a button or trigger a sensor. The point that moves is named here.
              </span>
            </>
          )}
        </div>
        {latest && onName && (
          <button className="btn primary" onClick={() => onName(latest.name)}>
            Name this
          </button>
        )}
      </div>

      <div className="card">
        <div className="card-head">
          <h2>
            Signal log {source.length > 0 && <small>{visible.length} of {source.length}</small>}
          </h2>
          <div className="button-row">
            <button className={`btn ${frozen ? 'primary' : ''}`} onClick={() => setFrozen(frozen ? null : log)}>
              {frozen ? 'Resume' : 'Pause'}
            </button>
            <button className="btn" onClick={onClear} disabled={log.length === 0}>
              Clear
            </button>
          </div>
        </div>

        <div className="plc-filters">
          <input
            type="search"
            placeholder="Filter by name, label or location"
            value={search}
            onChange={(e) => setSearch(e.target.value)}
          />
          <label className="checkbox">
            <input type="checkbox" checked={risingOnly} onChange={(e) => setRisingOnly(e.target.checked)} />
            <span>Presses only</span>
          </label>
          <label className="checkbox">
            <input type="checkbox" checked={hideMotion} onChange={(e) => setHideMotion(e.target.checked)} />
            <span>Hide motion</span>
          </label>
        </div>

        {shown.length === 0 ? (
          <Empty>
            {source.length === 0
              ? 'Nothing has changed yet. Only real transitions are logged, so the retained values that arrive when a broker connects are not listed here.'
              : 'Nothing matches these filters.'}
          </Empty>
        ) : (
          <>
            <div className="table-wrap">
              <table className="responsive">
                <thead>
                  <tr>
                    <th>Time</th>
                    <th>Point</th>
                    <th>Address</th>
                    <th>Location</th>
                    <th>Change</th>
                    {onName && <th aria-label="Actions" />}
                  </tr>
                </thead>
                <tbody>
                  {shown.map((e) => (
                    <tr key={e.seq}>
                      <td data-label="Time" className="mono">
                        {formatTime(e.at)}
                      </td>
                      <td data-label="Point">
                        {describe(e)}
                        {technicalName(e) && <small className="mono"> {technicalName(e)}</small>}
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
                      {onName && (
                        <td data-label="">
                          <button className="btn" onClick={() => onName(e.name)}>
                            Name
                          </button>
                        </td>
                      )}
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
            {visible.length > shown.length && (
              <div className="button-row">
                <button className="btn" onClick={() => setLimit((n) => n + discoveryPageSize)}>
                  Show {Math.min(discoveryPageSize, visible.length - shown.length)} more
                </button>
              </div>
            )}
          </>
        )}
      </div>
    </>
  )
}

/** pointsPageSize keeps a three-hundred-row table from becoming the page. */
const pointsPageSize = 50

function PointsView({ points, onName }: { points: PlcPoint[]; onName?: (name: string) => void }) {
  const [search, setSearch] = useState('')
  const [kind, setKind] = useState<'' | 'input' | 'output' | 'temperature'>('')
  const [location, setLocation] = useState('')
  const [limit, setLimit] = useState(pointsPageSize)

  const locations = Array.from(new Set(points.map((p) => p.location).filter(Boolean) as string[])).sort()
  const counts = {
    input: points.filter((p) => p.kind === 'input').length,
    output: points.filter((p) => p.kind === 'output').length,
    temperature: points.filter((p) => p.kind === 'temperature').length,
  }

  const needle = search.trim().toLowerCase()
  const visible = points.filter((p) => {
    if (kind && p.kind !== kind) return false
    if (location && p.location !== location) return false
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

      <div className="plc-filters">
        <input
          type="search"
          placeholder="Search name, label or location"
          value={search}
          onChange={(e) => setSearch(e.target.value)}
        />
        {locations.length > 0 && (
          <select value={location} onChange={(e) => setLocation(e.target.value)}>
            <option value="">Everywhere</option>
            {locations.map((l) => (
              <option key={l} value={l}>
                {l}
              </option>
            ))}
          </select>
        )}
      </div>
      <Segmented
        value={kind}
        onChange={setKind}
        options={[
          { value: '', label: 'All', count: points.length },
          { value: 'input', label: 'Inputs', count: counts.input },
          { value: 'output', label: 'Outputs', count: counts.output },
          { value: 'temperature', label: 'Temps', count: counts.temperature },
        ]}
      />

      {visible.length === 0 ? (
        <Empty>Nothing matches.</Empty>
      ) : (
        <>
        <div className="table-wrap">
          <table className="responsive">
            <thead>
              <tr>
                <th>Point</th>
                <th>Kind</th>
                <th>Address</th>
                <th>Location</th>
                <th>Value</th>
                <th>Updated</th>
                {onName && <th aria-label="Actions" />}
              </tr>
            </thead>
            <tbody>
              {visible.slice(0, limit).map((p) => (
                <tr key={p.topic}>
                  <td data-label="Point">
                    {describe(p)}
                    {technicalName(p) && <small className="mono"> {technicalName(p)}</small>}
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
                  {onName && (
                    <td data-label="">
                      <button className="btn" onClick={() => onName(p.name)}>
                        Name
                      </button>
                    </td>
                  )}
                </tr>
              ))}
            </tbody>
          </table>
        </div>
        {visible.length > limit && (
          <div className="button-row">
            <button className="btn" onClick={() => setLimit((n) => n + pointsPageSize)}>
              Show {Math.min(pointsPageSize, visible.length - limit)} more
            </button>
          </div>
        )}
        </>
      )}
    </div>
  )
}

type LightFilter = 'all' | 'on' | 'fault'

function LightsView({
  state,
  commands,
  canOperate,
  onSend,
}: {
  state: PlcState
  commands: PlcCommands | null
  canOperate: boolean
  onSend: (target: string, command: string, address?: number, params?: string[]) => Promise<void>
}) {
  const { lights, summary } = state
  const [filter, setFilter] = useState<LightFilter>('all')
  const [search, setSearch] = useState('')
  const [open, setOpen] = useState<number | null>(null)
  const [busy, setBusy] = useState(false)

  // Control needs both the operator role and the plugin setting: the setting is
  // what the house owner turned on, the role is who may use it.
  const control = canOperate && Boolean(commands?.allowControl)

  const needle = search.trim().toLowerCase()
  const visible = lights.filter((l) => {
    if (filter === 'on' && l.actualLevel <= 0) return false
    if (filter === 'fault' && !l.error) return false
    if (!needle) return true
    return [l.name, String(l.address)].some((f) => f.toLowerCase().includes(needle))
  })

  const run = async (address: number | null, target: string, command: string, params?: string[]) => {
    setBusy(true)
    try {
      await onSend(target, command, address ?? undefined, params)
    } finally {
      setBusy(false)
    }
  }

  return (
    <div className="card">
      <div className="card-head">
        <h2>DALI lights</h2>
        {control && (
          <div className="button-row">
            <button className="btn" disabled={busy} onClick={() => run(null, 'dali', 'refresh')}>
              Refresh ballasts
            </button>
            <button className="btn" disabled={busy} onClick={() => run(null, 'system', 'refresh')}>
              Refresh PLC
            </button>
          </div>
        )}
      </div>

      {!control && (
        <Alert kind="info">
          {canOperate ? (
            <>
              Control is switched off. Turn on <strong>Allow DALI control</strong> in the plugin&apos;s settings to
              switch and dim these from here.
            </>
          ) : (
            <>Switching lights needs the operator role.</>
          )}
        </Alert>
      )}

      {summary.lightsWithError > 0 && (
        <p className="subtitle">
          {summary.lightsWithError} of {summary.lights} ballasts are not answering the PLC. That is a bus or
          addressing matter rather than something to fix here.
        </p>
      )}

      <div className="plc-filters">
        <input
          type="search"
          placeholder="Find a light"
          value={search}
          onChange={(e) => setSearch(e.target.value)}
        />
      </div>
      <Segmented
        value={filter}
        onChange={setFilter}
        options={[
          { value: 'all', label: 'All', count: summary.lights },
          { value: 'on', label: 'On', count: summary.lightsOn },
          { value: 'fault', label: 'Not answering', count: summary.lightsWithError },
        ]}
      />

      {visible.length === 0 ? (
        <Empty>No lights match.</Empty>
      ) : (
        <ul className="plc-list">
          {visible.map((l) => {
            const percent = l.actualLevel > 0 ? Math.round((l.actualLevel / 254) * 100) : 0
            const fault = daliFault(l.error)
            const expanded = open === l.address
            return (
              <li key={l.address} className={fault ? 'muted' : ''}>
                <button
                  className={`plc-row ${expanded ? 'open' : ''}`}
                  onClick={() => setOpen(expanded ? null : l.address)}
                  aria-expanded={expanded}
                >
                  <span className={`plc-dot ${percent > 0 ? 'lit' : ''}`} />
                  <span className="plc-row-name mono">{l.name || `LI-${l.address}`}</span>
                  <span className="plc-bar">
                    <span style={{ width: `${percent}%` }} />
                  </span>
                  <span className="plc-row-pct mono">{percent}%</span>
                  {fault && (
                    <span className="plc-fault" title={l.error}>
                      {fault}
                    </span>
                  )}
                </button>

                {expanded && (
                  <div className="plc-light-panel">
                    <dl className="plc-facts">
                      <div>
                        <dt>Address</dt>
                        <dd className="mono">{l.address}</dd>
                      </div>
                      <div>
                        <dt>Level</dt>
                        <dd className="mono">{l.actualLevel} of 254</dd>
                      </div>
                      <div>
                        <dt>Min / max</dt>
                        <dd className="mono">
                          {l.minLevel} / {l.maxLevel}
                        </dd>
                      </div>
                      <div>
                        <dt>Last command</dt>
                        <dd>{l.lastCommand || '—'}</dd>
                      </div>
                    </dl>
                    {control ? (
                      <>
                        <div className="button-row">
                          <button className="btn" disabled={busy} onClick={() => run(l.address, 'dali', 'on')}>
                            On
                          </button>
                          <button className="btn" disabled={busy} onClick={() => run(l.address, 'dali', 'off')}>
                            Off
                          </button>
                          <button
                            className="btn"
                            disabled={busy}
                            onClick={() => run(l.address, 'dali', 'query_actual_level')}
                          >
                            Query
                          </button>
                        </div>
                        <label className="plc-slider">
                          <span>Level</span>
                          <input
                            type="range"
                            min={0}
                            max={254}
                            defaultValue={l.actualLevel}
                            disabled={busy}
                            aria-label={`Level for ${l.name || l.address}`}
                            // Fires on release rather than on drag: every step
                            // would otherwise be its own command on the bus.
                            onMouseUp={(e) => run(l.address, 'dali', 'arc', [e.currentTarget.value])}
                            onTouchEnd={(e) => run(l.address, 'dali', 'arc', [e.currentTarget.value])}
                          />
                        </label>
                      </>
                    ) : null}
                  </div>
                )}
              </li>
            )
          })}
        </ul>
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
            <h2>
              Controller <small>updated {formatRelative(watchdog.updatedAt)}</small>
            </h2>
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

      {electricity.map((e) => {
        const meanCurrent = e.phases.reduce((sum, p) => sum + p.current, 0) / e.phases.length
        return (
        <div className="card" key={e.name}>
          <div className="card-head">
            <h2>Electricity — {e.name}</h2>
            {e.alarmActive && <span className="badge err">alarm</span>}
          </div>
          <div className="table-wrap">
            <table className="responsive">
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
              {/* Not coloured by a threshold of our own: the PLC computes this
                  figure and raises its own alarm from it, and inventing a
                  second opinion here would only disagree with the controller. */}
              <dd className="mono">{e.currentImbalance.toFixed(1)} %</dd>
            </div>
          </dl>
          <p className="subtitle">
            Imbalance is relative, so at the {meanCurrent.toFixed(1)} A these phases are drawing, one extra lamp
            on one phase reads as a large percentage. The controller raises its own alarm when it matters.
          </p>
          {e.activeAlarms && e.activeAlarms.length > 0 && (
            <Alert kind="error">{e.activeAlarms.join(', ')}</Alert>
          )}
        </div>
        )
      })}

      {meters.map((m) => (
        <div className="card" key={m.name}>
          <div className="card-head">
            <h2>{m.name.replace(/_/g, ' ')}</h2>
            <span className={`badge ${m.available ? 'ok' : 'err'}`}>{m.available ? 'online' : 'offline'}</span>
          </div>
          {Object.keys(m.readings ?? {}).length === 0 ? (
            <Empty>
              No readings yet. This meter does not publish its state as a retained message, so nothing is known
              about it until it next reports.
            </Empty>
          ) : (
            <dl className="plc-facts">
              {Object.entries(m.readings ?? {}).map(([k, v]) => (
                <div key={k}>
                  <dt>{k.replace(/_/g, ' ')}</dt>
                  <dd className="mono">
                    {v}
                    {/* Units come from the plugin, so the panel, the API and the
                        MCP server all report the same thing. */}
                    {m.units?.[k] ? ` ${m.units[k]}` : ''}
                  </dd>
                </div>
              ))}
            </dl>
          )}
        </div>
      ))}

      {electricity.length === 0 && meters.length === 0 && !watchdog && (
        <Empty>No power or metering data has arrived yet.</Empty>
      )}
    </>
  )
}
