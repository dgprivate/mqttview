import { useCallback, useEffect, useState } from 'react'
import { Link, useNavigate, useParams } from 'react-router-dom'
import { api } from '../api/client'
import type { GraphNode, TopicGraph as Graph } from '../api/types'
import { Alert, Spinner } from '../components/common'

/**
 * TopicGraph draws the namespace as nested blocks rather than a node-and-edge
 * diagram.
 *
 * A force-directed graph of a broker's namespace looks impressive and answers
 * nothing: the interesting facts are which branch carries the traffic and
 * which has gone quiet, and both are easier to read as size and colour in a
 * layout that does not move. It also survives a phone screen, which a graph
 * with dragging nodes does not.
 */
export function TopicGraph() {
  const { id = '' } = useParams<{ id: string }>()
  const navigate = useNavigate()

  const [graph, setGraph] = useState<Graph | null>(null)
  const [error, setError] = useState('')
  const [depth, setDepth] = useState(3)

  const load = useCallback(async () => {
    try {
      setGraph(await api.graph(id, depth, 500))
      setError('')
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Could not load the graph')
    }
  }, [id, depth])

  useEffect(() => {
    void load()
  }, [load])

  useEffect(() => {
    const timer = setInterval(() => void load(), 5000)
    return () => clearInterval(timer)
  }, [load])

  if (!graph) {
    return error ? <Alert kind="error">{error}</Alert> : <Spinner label="Reading the namespace…" />
  }

  const top = graph.nodes.filter((n) => n.depth === 1)
  const busiest = Math.max(1, ...graph.nodes.map((n) => n.messages))
  const newest = graph.newest ? new Date(graph.newest).getTime() : Date.now()

  return (
    <>
      <div className="page-head">
        <div>
          <h1>Namespace</h1>
          <p className="subtitle">
            {graph.topics.toLocaleString()} topics · {graph.messages.toLocaleString()} messages
            {graph.countingSince && ` since ${new Date(graph.countingSince).toLocaleString()}`}
          </p>
        </div>
        <div className="button-row">
          <Link className="button small" to={`/connections/${id}`}>
            Explorer
          </Link>
        </div>
      </div>

      {error && <Alert kind="error">{error}</Alert>}
      {graph.treeFull && (
        <Alert kind="info">
          The topic tree is full, so new topics are no longer being recorded. What is drawn here is
          everything mqttview saw before that point.
        </Alert>
      )}
      {graph.truncated && (
        <Alert kind="info">
          Too many branches to draw at this depth. The busiest are kept, because that is the
          question a picture of a namespace is drawn to answer.
        </Alert>
      )}

      <div className="field-row two">
        <div className="field">
          <label htmlFor="graph-depth">Depth</label>
          <select id="graph-depth" value={depth} onChange={(e) => setDepth(Number(e.target.value))}>
            <option value={1}>1 level</option>
            <option value={2}>2 levels</option>
            <option value={3}>3 levels</option>
            <option value={4}>4 levels</option>
          </select>
        </div>
      </div>

      <p className="subtitle">
        Colour is how recently a branch last changed, and on a wide screen width is how much
        traffic it carries. Select one to open it in the explorer.
      </p>

      {top.length === 0 ? (
        <p className="subtitle">Nothing has arrived on this broker yet.</p>
      ) : (
        <div className="graph">
          {top.map((node) => (
            <Branch
              key={node.topic}
              node={node}
              all={graph.nodes}
              busiest={busiest}
              newest={newest}
              onSelect={(topic) => navigate(`/connections/${id}?topic=${encodeURIComponent(topic)}`)}
            />
          ))}
        </div>
      )}
    </>
  )
}

function Branch({
  node,
  all,
  busiest,
  newest,
  onSelect,
}: {
  node: GraphNode
  all: GraphNode[]
  busiest: number
  newest: number
  onSelect: (topic: string) => void
}) {
  const children = all.filter(
    (n) => n.depth === node.depth + 1 && n.topic.startsWith(`${node.topic}/`),
  )

  // Square-rooted, so a branch with a hundred times the traffic is ten times
  // the size rather than a hundred: the loud one has to stand out without the
  // quiet ones becoming invisible.
  const share = Math.sqrt(node.messages / busiest)
  const minWidth = 120 + share * 260

  return (
    <div className="graph-branch" style={{ minWidth: `${Math.round(minWidth)}px` }}>
      <button
        className="graph-node"
        style={{ borderLeftColor: recencyColour(node.updatedAt, newest) }}
        onClick={() => onSelect(node.topic)}
        title={node.topic}
      >
        <span className="graph-name mono">{node.name}</span>
        <span className="graph-meta">
          {node.messages.toLocaleString()} msg · {node.topics.toLocaleString()} topics
          {node.truncated && ' · more below'}
        </span>
      </button>
      {children.length > 0 && (
        <div className="graph-children">
          {children.map((child) => (
            <Branch
              key={child.topic}
              node={child}
              all={all}
              busiest={busiest}
              newest={newest}
              onSelect={onSelect}
            />
          ))}
        </div>
      )}
    </div>
  )
}

/**
 * recencyColour goes from the accent colour for something that just changed to
 * a muted grey for something that has not changed in a day.
 *
 * A branch that has gone silent is the thing somebody is usually looking for,
 * and it is much easier to spot as a colour than as a timestamp in a list.
 */
function recencyColour(updatedAt: string | undefined, newest: number): string {
  if (!updatedAt) return 'var(--text-dim)'
  const age = (newest - new Date(updatedAt).getTime()) / 1000
  if (age < 60) return 'var(--ok)'
  if (age < 15 * 60) return 'var(--accent)'
  if (age < 60 * 60) return 'var(--warn)'
  return 'var(--text-dim)'
}
