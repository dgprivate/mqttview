import { useCallback, useEffect, useState } from 'react'
import { Link, useParams } from 'react-router-dom'
import { api } from '../api/client'
import type { BrokerStatsResponse, Connection, ConnectionInput } from '../api/types'
import { useAuth } from '../auth/AuthContext'
import { Alert, formatBytes, formatTime, Spinner } from '../components/common'

/**
 * BrokerStatus shows what the broker publishes about itself under $SYS.
 *
 * Every number here is the broker's own. There is no rate computed in
 * mqttview sitting beside the broker's, because the broker is the authority
 * on its own load and two disagreeing numbers on one page is worse than one.
 */
export function BrokerStatus() {
  const { id = '' } = useParams<{ id: string }>()
  const { can } = useAuth()

  const [data, setData] = useState<BrokerStatsResponse | null>(null)
  const [connection, setConnection] = useState<Connection | null>(null)
  const [error, setError] = useState('')
  const [busy, setBusy] = useState(false)

  const load = useCallback(async () => {
    try {
      const [stats, conn] = await Promise.all([api.brokerStats(id), api.connection(id)])
      setData(stats)
      setConnection(conn)
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Could not load the broker statistics')
    }
  }, [id])

  useEffect(() => {
    void load()
  }, [load])

  // $SYS is republished every few seconds, so a page that never refreshes is
  // showing a moment that has passed.
  useEffect(() => {
    const timer = setInterval(() => void load(), 5000)
    return () => clearInterval(timer)
  }, [load])

  async function toggleCollection(enabled: boolean) {
    if (!connection) return
    setBusy(true)
    setError('')
    try {
      await api.updateConnection(id, { ...toInput(connection), sysStats: enabled })
      await load()
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Could not change the setting')
    } finally {
      setBusy(false)
    }
  }

  if (!data || !connection) {
    return error ? <Alert kind="error">{error}</Alert> : <Spinner label="Reading the broker…" />
  }

  const stats = data.stats

  return (
    <>
      <div className="page-head">
        <div>
          <h1>{connection.name}</h1>
          <p className="subtitle">What the broker says about itself</p>
        </div>
        <div className="button-row">
          <Link className="btn small" to={`/connections/${id}`}>
            Explorer
          </Link>
        </div>
      </div>

      {error && <Alert kind="error">{error}</Alert>}

      {!data.enabled && (
        <Alert kind="info">
          Broker statistics are off for this connection. Turning them on holds a{' '}
          <code>$SYS/#</code> subscription open — a plain wildcard does not reach that namespace, so
          nothing is collected until it is asked for.
          {can('admin') && (
            <>
              {' '}
              <button className="small" onClick={() => toggleCollection(true)} disabled={busy}>
                {busy ? 'Turning on…' : 'Turn on'}
              </button>
            </>
          )}
        </Alert>
      )}

      {data.enabled && !stats.available && (
        <Alert kind="info">
          {data.connected
            ? 'Nothing has arrived under $SYS yet. Either the broker publishes none, or it is configured to deny the reserved namespace — this is not the same as a broker with nothing to report.'
            : 'The connection is not connected, so no statistics are arriving.'}
        </Alert>
      )}

      {stats.available && (
        <>
          <p className="subtitle">
            {stats.version && <>{stats.version} · </>}
            {stats.hasUptime && <>up {formatDuration(stats.uptime ?? 0)} · </>}
            last value {stats.updatedAt ? formatTime(stats.updatedAt) : 'unknown'}
            {can('admin') && data.enabled && (
              <>
                {' · '}
                <button className="link" onClick={() => toggleCollection(false)} disabled={busy}>
                  stop collecting
                </button>
              </>
            )}
          </p>

          <div className="tiles">
            <Tile label="Clients connected" value={stats.clients.connected} />
            <Tile label="Clients total" value={stats.clients.total} />
            <Tile label="Peak clients" value={stats.clients.maximum} />
            <Tile label="Subscriptions" value={stats.subscriptions} />
            <Tile label="Retained messages" value={stats.retained} />
            <Tile label="Messages stored" value={stats.messages.stored} />
            <Tile label="Messages received" value={stats.messages.received} />
            <Tile label="Messages sent" value={stats.messages.sent} />
            <Tile label="Messages dropped" value={stats.messages.dropped} />
            <Tile label="Bytes received" value={stats.bytes.received} format={formatBytes} />
            <Tile label="Bytes sent" value={stats.bytes.sent} format={formatBytes} />
            <Tile label="Heap" value={stats.heap.current} format={formatBytes} />
          </div>

          {stats.load && Object.keys(stats.load).length > 0 && (
            <div className="card">
              <h2>Load</h2>
              <p className="subtitle">
                The broker's own moving averages, as it publishes them.
              </p>
              <table className="responsive">
                <thead>
                  <tr>
                    <th>Measure</th>
                    <th>Value</th>
                  </tr>
                </thead>
                <tbody>
                  {Object.entries(stats.load)
                    .sort(([a], [b]) => a.localeCompare(b))
                    .map(([key, value]) => (
                      <tr key={key}>
                        <td data-label="Measure" className="mono">
                          {key}
                        </td>
                        <td data-label="Value">{value.toLocaleString()}</td>
                      </tr>
                    ))}
                </tbody>
              </table>
            </div>
          )}

          {stats.raw && Object.keys(stats.raw).length > 0 && (
            <details className="card">
              <summary>
                <h2 style={{ display: 'inline' }}>Everything published ({Object.keys(stats.raw).length})</h2>
              </summary>
              <p className="subtitle">
                Every $SYS topic exactly as the broker published it, so a broker whose names nobody
                here has seen still shows what it reports.
              </p>
              <table className="responsive">
                <thead>
                  <tr>
                    <th>Topic</th>
                    <th>Value</th>
                  </tr>
                </thead>
                <tbody>
                  {Object.entries(stats.raw)
                    .sort(([a], [b]) => a.localeCompare(b))
                    .map(([key, value]) => (
                      <tr key={key}>
                        <td data-label="Topic" className="mono">
                          {key}
                        </td>
                        <td data-label="Value" className="mono">
                          {value}
                        </td>
                      </tr>
                    ))}
                </tbody>
              </table>
            </details>
          )}
        </>
      )}
    </>
  )
}

/** Tile shows one number, or says the broker did not report it. An absent
 *  value is not a zero. */
function Tile({
  label,
  value,
  format,
}: {
  label: string
  value?: number
  format?: (n: number) => string
}) {
  return (
    <div className="tile">
      <span className="tile-label">{label}</span>
      <span className="tile-value">
        {value === undefined ? (
          <span className="subtitle">not reported</span>
        ) : format ? (
          format(value)
        ) : (
          value.toLocaleString()
        )}
      </span>
    </div>
  )
}

function formatDuration(seconds: number): string {
  const days = Math.floor(seconds / 86400)
  const hours = Math.floor((seconds % 86400) / 3600)
  const minutes = Math.floor((seconds % 3600) / 60)
  if (days > 0) return `${days}d ${hours}h`
  if (hours > 0) return `${hours}h ${minutes}m`
  return `${minutes}m`
}

/**
 * toInput turns a connection back into the shape an update accepts.
 *
 * The view carries fields the request does not, and the API refuses unknown
 * ones, so this cannot be the object straight back.
 */
export function toInput(c: Connection): ConnectionInput {
  return {
    name: c.name,
    url: c.url,
    version: c.version,
    clientId: c.clientId,
    username: c.username,
    keepAlive: c.keepAlive,
    cleanStart: c.cleanStart,
    sessionExpiry: c.sessionExpiry,
    connectTimeout: c.connectTimeout,
    // The view leaves absent strings undefined; the request wants them
    // present and empty. Sending undefined drops the field, which the server
    // reads as "no change" for some and as "clear it" for others.
    tls: {
      insecureSkipVerify: c.tls.insecureSkipVerify,
      serverName: c.tls.serverName ?? '',
      minVersion: c.tls.minVersion ?? '',
      alpn: c.tls.alpn ?? null,
    },
    will: c.will ?? null,
    subscriptions: c.subscriptions,
    autoConnect: c.autoConnect,
    historySize: c.historySize,
    topicLogEntries: c.topicLogEntries,
    topicLogBudget: c.topicLogBudget,
    sysStats: c.sysStats,
    recordToDisk: c.recordToDisk,
    recordKeep: c.recordKeep,
  }
}
