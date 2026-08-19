import { useCallback, useEffect, useState } from 'react'
import { api } from '../api/client'
import type { TreeNode } from '../api/types'

interface Props {
  connectionId: string
  selected: string
  onSelect: (topic: string) => void
  /** refreshKey changes to force a reload, e.g. when new messages arrive. */
  refreshKey: number
}

/**
 * TopicTree lazily loads one level at a time. A busy broker can hold hundreds
 * of thousands of topics, so the browser is never given the whole tree.
 */
export function TopicTree({ connectionId, selected, onSelect, refreshKey }: Props) {
  return (
    <div className="tree">
      <TreeLevel
        connectionId={connectionId}
        prefix=""
        depth={0}
        selected={selected}
        onSelect={onSelect}
        refreshKey={refreshKey}
      />
    </div>
  )
}

interface LevelProps extends Props {
  prefix: string
  depth: number
}

function TreeLevel({ connectionId, prefix, depth, selected, onSelect, refreshKey }: LevelProps) {
  const [nodes, setNodes] = useState<TreeNode[] | null>(null)
  const [expanded, setExpanded] = useState<Set<string>>(new Set())
  const [error, setError] = useState('')

  const load = useCallback(async () => {
    try {
      const result = await api.tree(connectionId, prefix)
      setNodes(result.children)
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Could not load topics')
    }
  }, [connectionId, prefix])

  useEffect(() => {
    void load()
  }, [load, refreshKey])

  // Keep a path to the selected topic open so a search result is visible.
  useEffect(() => {
    if (!selected || !nodes) return
    setExpanded((prev) => {
      const next = new Set(prev)
      for (const node of nodes) {
        if (selected === node.topic || selected.startsWith(node.topic + '/')) {
          next.add(node.topic)
        }
      }
      return next
    })
  }, [selected, nodes])

  if (error) return <p className="subtitle">{error}</p>
  if (!nodes) return depth === 0 ? <p className="subtitle">Loading topics…</p> : null
  if (nodes.length === 0 && depth === 0) {
    return <p className="subtitle">No topics received yet.</p>
  }

  return (
    <div className={depth > 0 ? 'tree-children' : undefined}>
      {nodes.map((node) => {
        const isOpen = expanded.has(node.topic)
        const hasChildren = node.childCount > 0
        return (
          <div key={node.topic}>
            <button
              type="button"
              className={`tree-node ${selected === node.topic ? 'selected' : ''}`}
              onClick={() => {
                if (hasChildren) {
                  setExpanded((prev) => {
                    const next = new Set(prev)
                    if (next.has(node.topic)) next.delete(node.topic)
                    else next.add(node.topic)
                    return next
                  })
                }
                if (node.value) onSelect(node.topic)
              }}
            >
              <span className="twist">{hasChildren ? (isOpen ? '▾' : '▸') : '·'}</span>
              <span className="label">{node.name}</span>
              {node.value && <span className="badge">{node.value.count.toLocaleString()}</span>}
              {hasChildren && <span className="count">{node.topicCount.toLocaleString()}</span>}
            </button>

            {isOpen && (
              <TreeLevel
                connectionId={connectionId}
                prefix={node.topic}
                depth={depth + 1}
                selected={selected}
                onSelect={onSelect}
                refreshKey={refreshKey}
              />
            )}
          </div>
        )
      })}
    </div>
  )
}
