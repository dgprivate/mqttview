import type { ReactNode } from 'react'
import type { ConnectionState, Status } from '../api/types'

/** StatusBadge renders a connection's state with a colour that matches it. */
export function StatusBadge({ status }: { status: Status | undefined }) {
  const state: ConnectionState = status?.state ?? 'disconnected'
  const tone = state === 'connected' ? 'ok' : state === 'error' ? 'err' : state === 'connecting' ? 'warn' : ''

  return (
    <span className={`badge ${tone}`} title={status?.lastError ?? ''}>
      <span className="dot" />
      {state}
    </span>
  )
}

export function Alert({ kind, children }: { kind: 'error' | 'info' | 'ok'; children: ReactNode }) {
  if (!children) return null
  return <div className={`alert ${kind}`}>{children}</div>
}

export function Spinner({ label }: { label?: string }) {
  return (
    <div className="empty">
      <span className="spinner" /> {label ?? 'Loading…'}
    </div>
  )
}

export function Empty({ children }: { children: ReactNode }) {
  return <div className="empty">{children}</div>
}

/** formatTime renders a timestamp as local wall-clock time with milliseconds. */
export function formatTime(iso: string): string {
  const d = new Date(iso)
  if (Number.isNaN(d.getTime())) return iso
  return d.toLocaleTimeString(undefined, { hour12: false }) + '.' + String(d.getMilliseconds()).padStart(3, '0')
}

/** formatRelative renders how long ago something happened, briefly. */
export function formatRelative(iso: string | undefined): string {
  if (!iso) return 'never'
  const then = new Date(iso).getTime()
  if (Number.isNaN(then)) return iso
  const seconds = Math.max(0, Math.round((Date.now() - then) / 1000))
  if (seconds < 60) return `${seconds}s ago`
  if (seconds < 3600) return `${Math.round(seconds / 60)}m ago`
  if (seconds < 86400) return `${Math.round(seconds / 3600)}h ago`
  return `${Math.round(seconds / 86400)}d ago`
}

export function formatBytes(n: number): string {
  if (n < 1024) return `${n} B`
  if (n < 1024 * 1024) return `${(n / 1024).toFixed(1)} KiB`
  return `${(n / (1024 * 1024)).toFixed(1)} MiB`
}

/**
 * prettyPayload pretty-prints JSON payloads and leaves everything else alone.
 * MQTT carries a lot of JSON, and unformatted JSON is unreadable on a phone.
 */
export function prettyPayload(text: string): string {
  const trimmed = text.trim()
  if (!trimmed.startsWith('{') && !trimmed.startsWith('[')) return text
  try {
    return JSON.stringify(JSON.parse(trimmed), null, 2)
  } catch {
    return text
  }
}
