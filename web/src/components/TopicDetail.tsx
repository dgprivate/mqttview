import { useCallback, useEffect, useState } from 'react'
import { api } from '../api/client'
import type {
  HistoryEntry,
  SeriesPoint,
  TopicDecode,
  TopicDiff,
  TopicHistory,
  TreeNode,
} from '../api/types'
import { useAuth } from '../auth/AuthContext'
import { Alert, formatBytes, formatTime, prettyPayload } from './common'
import { Chart } from './Chart'

type Tab = 'value' | 'timeline' | 'diff' | 'chart'

/**
 * TopicDetail is everything known about one topic: its current value, its own
 * recent past, what changed since the message before, and a chart of any
 * number inside it.
 *
 * The tabs are lazy. A busy install has thousands of topics and clicking one
 * should not fetch four things, three of which nobody looks at.
 */
export function TopicDetail({
  connectionId,
  topic,
  node,
  onChanged,
}: {
  connectionId: string
  topic: string
  node: TreeNode | null
  onChanged: () => void
}) {
  const { can } = useAuth()
  const [tab, setTab] = useState<Tab>('value')
  const [decoded, setDecoded] = useState<TopicDecode | null>(null)
  const [error, setError] = useState('')

  // Reset to the value tab when the topic changes: staying on "chart" while
  // moving to a topic that has no numbers shows an empty panel and looks
  // broken.
  useEffect(() => {
    setTab('value')
    setDecoded(null)
    setError('')
  }, [topic])

  useEffect(() => {
    if (!topic) return
    let cancelled = false
    api
      .topicDecode(connectionId, topic)
      .then((d) => {
        if (!cancelled) setDecoded(d)
      })
      .catch(() => undefined)
    return () => {
      cancelled = true
    }
  }, [connectionId, topic, node?.value?.updatedAt])

  if (!topic) return null

  return (
    <div className="card">
      <div className="card-head">
        <h2 className="mono topic-heading">{topic}</h2>
        <TopicActions
          connectionId={connectionId}
          topic={topic}
          canOperate={can('operator')}
          onChanged={onChanged}
          onError={setError}
        />
      </div>

      {node?.value && (
        <p className="subtitle">
          Updated {formatTime(node.value.updatedAt)} · {node.value.count.toLocaleString()} messages ·{' '}
          {formatBytes(node.value.size)}
          {node.value.retain && ' · retained'}
          {decoded && ` · ${decoded.detected.kind}`}
        </p>
      )}

      {error && <Alert kind="error">{error}</Alert>}

      <div className="tabs" role="tablist">
        {(['value', 'timeline', 'diff', 'chart'] as Tab[]).map((t) => (
          <button
            key={t}
            role="tab"
            aria-selected={tab === t}
            className={tab === t ? 'tab active' : 'tab'}
            onClick={() => setTab(t)}
          >
            {t === 'value' ? 'Value' : t === 'timeline' ? 'Timeline' : t === 'diff' ? 'Changes' : 'Chart'}
          </button>
        ))}
      </div>

      {tab === 'value' && <ValueTab connectionId={connectionId} topic={topic} node={node} decoded={decoded} />}
      {tab === 'timeline' && <TimelineTab connectionId={connectionId} topic={topic} />}
      {tab === 'diff' && <DiffTab connectionId={connectionId} topic={topic} />}
      {tab === 'chart' && <ChartTab connectionId={connectionId} topic={topic} />}
    </div>
  )
}

/** TopicActions are the things done to a topic rather than read from it. */
function TopicActions({
  connectionId,
  topic,
  canOperate,
  onChanged,
  onError,
}: {
  connectionId: string
  topic: string
  canOperate: boolean
  onChanged: () => void
  onError: (message: string) => void
}) {
  const [clearing, setClearing] = useState(false)

  async function clearRetained() {
    // Naming the topic in the question, because the whole risk of this button
    // is clearing the wrong one — and the value cannot be put back.
    const ok = window.confirm(
      `Clear the retained message on ${topic}?\n\n` +
        'The broker forgets it for every client that connects afterwards. This cannot be undone.',
    )
    if (!ok) return

    setClearing(true)
    try {
      const result = await api.clearRetained(connectionId, topic)
      if (!result.hadRetainedValue) {
        onError('Sent — though mqttview had no retained value recorded for that topic.')
      }
      onChanged()
    } catch (err) {
      onError(err instanceof Error ? err.message : 'Could not clear the retained message')
    } finally {
      setClearing(false)
    }
  }

  return (
    <div className="button-row">
      <a className="btn small" href={api.topicExportURL(connectionId, topic, 'csv')} download>
        Export CSV
      </a>
      <a className="btn small" href={api.topicExportURL(connectionId, topic, 'json')} download>
        JSON
      </a>
      {canOperate && (
        <button className="small danger" onClick={clearRetained} disabled={clearing}>
          {clearing ? 'Clearing…' : 'Clear retained'}
        </button>
      )}
    </div>
  )
}

function ValueTab({
  connectionId,
  topic,
  node,
  decoded,
}: {
  connectionId: string
  topic: string
  node: TreeNode | null
  decoded: TopicDecode | null
}) {
  if (!node?.value) {
    return <p className="subtitle">This is a branch of the tree, not a topic with a value.</p>
  }

  // An image is shown rather than described. The server decides whether the
  // bytes are a renderable image; anything else it serves as a download, so
  // this cannot be talked into rendering a document.
  if (decoded?.detected.kind === 'image') {
    return (
      <div>
        <img
          className="payload-image"
          src={api.topicRawURL(connectionId, topic)}
          alt={`Payload of ${topic}`}
        />
        <p className="subtitle">
          {decoded.detected.mediaType} · {formatBytes(decoded.detected.size)}
        </p>
      </div>
    )
  }

  if (decoded?.detected.kind === 'sparkplug') {
    return <SparkplugView decoded={decoded} />
  }

  return (
    <>
      <pre className="payload">{prettyPayload(payloadText(node)) || '(empty payload)'}</pre>
      {node.value.truncated && <p className="subtitle">Payload truncated for display.</p>}
    </>
  )
}

/** payloadText decodes the tree's base64 payload for display. */
function payloadText(node: TreeNode): string {
  if (!node.value) return ''
  try {
    const binary = atob(node.value.payload)
    const bytes = Uint8Array.from(binary, (c) => c.charCodeAt(0))
    return new TextDecoder('utf-8', { fatal: false }).decode(bytes)
  } catch {
    return ''
  }
}

function SparkplugView({ decoded }: { decoded: TopicDecode }) {
  if (decoded.sparkplugError) {
    return (
      <Alert kind="error">
        This topic is in the Sparkplug namespace but the payload did not decode:{' '}
        {decoded.sparkplugError}
      </Alert>
    )
  }
  const payload = decoded.sparkplug
  if (!payload) return <p className="subtitle">Nothing decoded.</p>

  return (
    <>
      <p className="subtitle">
        {decoded.sparkplugTopic && (
          <>
            {decoded.sparkplugTopic.messageType} from {decoded.sparkplugTopic.edgeNode}
            {decoded.sparkplugTopic.device && ` / ${decoded.sparkplugTopic.device}`} in{' '}
            {decoded.sparkplugTopic.group}
          </>
        )}
        {payload.seq !== undefined && ` · sequence ${payload.seq}`}
      </p>
      {payload.metrics.length === 0 ? (
        <p className="subtitle">No metrics in this message.</p>
      ) : (
        <table className="responsive">
          <thead>
            <tr>
              <th>Metric</th>
              <th>Type</th>
              <th>Value</th>
            </tr>
          </thead>
          <tbody>
            {payload.metrics.map((m, i) => (
              <tr key={`${m.name ?? m.alias ?? i}`}>
                <td data-label="Metric" className="mono">
                  {m.name || (m.hasAlias ? `alias ${m.alias}` : '(unnamed)')}
                </td>
                <td data-label="Type">{m.dataType}</td>
                <td data-label="Value" className="mono">
                  {m.value !== undefined && m.value !== '' ? (
                    m.value
                  ) : (
                    <span className="subtitle">{m.note ?? '—'}</span>
                  )}
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
      {payload.bodyBytes ? (
        <p className="subtitle">
          Plus a {formatBytes(payload.bodyBytes)} body, which the specification leaves to the
          publisher to define.
        </p>
      ) : null}
    </>
  )
}

function TimelineTab({ connectionId, topic }: { connectionId: string; topic: string }) {
  const [history, setHistory] = useState<TopicHistory | null>(null)
  const [error, setError] = useState('')
  const [index, setIndex] = useState(0)

  const load = useCallback(() => {
    api
      .topicHistory(connectionId, topic)
      .then((h) => {
        setHistory(h)
        // Land on the newest, which is what somebody opening a timeline is
        // looking at before they start scrubbing.
        setIndex(Math.max(0, h.entries.length - 1))
      })
      .catch((err) => setError(err instanceof Error ? err.message : 'Could not load the history'))
  }, [connectionId, topic])

  useEffect(load, [load])

  if (error) return <Alert kind="error">{error}</Alert>
  if (!history) return <p className="subtitle">Loading…</p>
  if (history.entries.length === 0) {
    return <p className="subtitle">Nothing recorded for this topic yet.</p>
  }

  const entry = history.entries[Math.min(index, history.entries.length - 1)]

  return (
    <div className="timeline">
      <div className="field">
        <label htmlFor="timeline-scrub">
          Message {index + 1} of {history.entries.length} · {formatTime(entry.receivedAt)}
        </label>
        <input
          id="timeline-scrub"
          type="range"
          min={0}
          max={history.entries.length - 1}
          value={index}
          onChange={(e) => setIndex(Number(e.target.value))}
        />
      </div>

      <div className="button-row">
        <button className="small" onClick={() => setIndex((i) => Math.max(0, i - 1))} disabled={index === 0}>
          ← Older
        </button>
        <button
          className="small"
          onClick={() => setIndex((i) => Math.min(history.entries.length - 1, i + 1))}
          disabled={index >= history.entries.length - 1}
        >
          Newer →
        </button>
        <button className="small" onClick={load}>
          Refresh
        </button>
      </div>

      <EntryPayload entry={entry} />
    </div>
  )
}

function DiffTab({ connectionId, topic }: { connectionId: string; topic: string }) {
  const [diff, setDiff] = useState<TopicDiff | null>(null)
  const [error, setError] = useState('')

  useEffect(() => {
    api
      .topicDiff(connectionId, topic)
      .then(setDiff)
      .catch((err) => setError(err instanceof Error ? err.message : 'Could not load the comparison'))
  }, [connectionId, topic])

  if (error) return <Alert kind="error">{error}</Alert>
  if (!diff) return <p className="subtitle">Loading…</p>

  // Saying so rather than showing an empty comparison, which reads as "nothing
  // changed" when the truth is "there is nothing to compare against".
  if (!diff.previous) {
    return (
      <p className="subtitle">
        This topic has only been seen once, so there is nothing to compare against yet.
      </p>
    )
  }

  const before = entryText(diff.previous).split('\n')
  const after = entryText(diff.current).split('\n')
  const rows = diffLines(before, after)
  const changed = rows.some((r) => r.kind !== 'same')

  return (
    <>
      <p className="subtitle">
        Comparing {formatTime(diff.previous.receivedAt)} with {formatTime(diff.current.receivedAt)}
      </p>
      {!changed ? (
        <Alert kind="info">The payload is byte-for-byte the same as the message before it.</Alert>
      ) : (
        <pre className="payload diff">
          {rows.map((row, i) => (
            <div key={i} className={`diff-${row.kind}`}>
              <span className="diff-marker">
                {row.kind === 'added' ? '+' : row.kind === 'removed' ? '−' : ' '}
              </span>
              {row.text || ' '}
            </div>
          ))}
        </pre>
      )}
    </>
  )
}

/**
 * diffLines is a line-level comparison, and deliberately the simplest one that
 * is honest: common prefix, common suffix, and everything between marked as
 * replaced.
 *
 * A real edit-distance diff would find smaller changes in the middle, but MQTT
 * payloads are small and mostly JSON, and a wrong alignment in the middle of a
 * document reads as two unrelated changes rather than one.
 */
function diffLines(before: string[], after: string[]) {
  const rows: { kind: 'same' | 'added' | 'removed'; text: string }[] = []

  let start = 0
  while (start < before.length && start < after.length && before[start] === after[start]) {
    start++
  }
  let end = 0
  while (
    end < before.length - start &&
    end < after.length - start &&
    before[before.length - 1 - end] === after[after.length - 1 - end]
  ) {
    end++
  }

  for (let i = 0; i < start; i++) rows.push({ kind: 'same', text: before[i] })
  for (let i = start; i < before.length - end; i++) rows.push({ kind: 'removed', text: before[i] })
  for (let i = start; i < after.length - end; i++) rows.push({ kind: 'added', text: after[i] })
  for (let i = before.length - end; i < before.length; i++) rows.push({ kind: 'same', text: before[i] })

  return rows
}

function ChartTab({ connectionId, topic }: { connectionId: string; topic: string }) {
  const [fields, setFields] = useState<string[]>([])
  const [field, setField] = useState('')
  const [since, setSince] = useState('')
  const [points, setPoints] = useState<SeriesPoint[]>([])
  const [skipped, setSkipped] = useState(0)
  const [error, setError] = useState('')
  const [loaded, setLoaded] = useState(false)

  useEffect(() => {
    api
      .topicFields(connectionId, topic)
      .then((f) => {
        setFields(f.fields)
        setField((current) => (f.fields.includes(current) ? current : (f.fields[0] ?? '')))
        setLoaded(true)
      })
      .catch(() => setLoaded(true))
  }, [connectionId, topic])

  const load = useCallback(() => {
    api
      .topicSeries(connectionId, topic, field, since)
      .then((s) => {
        setPoints(s.points)
        setSkipped(s.skipped)
        setError('')
      })
      .catch((err) => setError(err instanceof Error ? err.message : 'Could not load the series'))
  }, [connectionId, topic, field, since])

  useEffect(() => {
    if (loaded && fields.length > 0) load()
  }, [loaded, fields.length, load])

  if (!loaded) return <p className="subtitle">Loading…</p>
  if (fields.length === 0) {
    return <p className="subtitle">Nothing in this topic's recent payloads is a number.</p>
  }

  return (
    <>
      <div className="field-row two">
        <div className="field">
          <label htmlFor="chart-field">Field</label>
          <select id="chart-field" value={field} onChange={(e) => setField(e.target.value)}>
            {fields.map((f) => (
              <option key={f} value={f}>
                {f === '' ? 'the payload itself' : f}
              </option>
            ))}
          </select>
        </div>
        <div className="field">
          <label htmlFor="chart-window">Window</label>
          <select id="chart-window" value={since} onChange={(e) => setSince(e.target.value)}>
            <option value="">everything kept</option>
            <option value="5m">last 5 minutes</option>
            <option value="1h">last hour</option>
            <option value="6h">last 6 hours</option>
            <option value="24h">last 24 hours</option>
          </select>
        </div>
      </div>

      {error && <Alert kind="error">{error}</Alert>}
      <Chart points={points} label={field || topic} />
      {skipped > 0 && (
        <p className="subtitle">
          {skipped.toLocaleString()} message{skipped === 1 ? '' : 's'} in this window had no number
          at that path and {skipped === 1 ? 'is' : 'are'} not plotted.
        </p>
      )}
      <div className="button-row">
        <button className="small" onClick={load}>
          Refresh
        </button>
      </div>
    </>
  )
}

function EntryPayload({ entry }: { entry: HistoryEntry }) {
  return (
    <>
      <pre className="payload">{prettyPayload(entryText(entry)) || '(empty)'}</pre>
      <p className="subtitle">
        {formatBytes(entry.size)} · QoS {entry.qos}
        {entry.retain && ' · retained'}
        {entry.base64 && ' · binary, shown as base64'}
        {entry.truncated && ' · truncated'}
      </p>
    </>
  )
}

/** entryText is the payload as text, whichever way it arrived. */
function entryText(entry: HistoryEntry): string {
  if (!entry.base64) return entry.payload
  return entry.payload
}
