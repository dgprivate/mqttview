import { useEffect, useRef, useState } from 'react'
import type { Message, Status } from '../api/types'

export interface Frame {
  type: 'hello' | 'message' | 'status' | 'event' | 'stats' | 'error' | 'pong'
  event?: string
  data?: unknown
}

type Listener = (frame: Frame) => void

/**
 * LiveSocket is a single WebSocket shared by the whole app. Every page
 * subscribes to it rather than opening its own, which keeps one connection per
 * tab and lets the server's per-client rate limit mean something.
 */
class LiveSocket {
  private socket: WebSocket | null = null
  private listeners = new Set<Listener>()
  private watches = new Map<string, string>()
  private reconnectDelay = 1000
  private reconnectTimer: number | null = null
  private closedByUs = false

  connect() {
    if (this.socket && (this.socket.readyState === WebSocket.OPEN || this.socket.readyState === WebSocket.CONNECTING)) {
      return
    }
    this.closedByUs = false

    const proto = window.location.protocol === 'https:' ? 'wss:' : 'ws:'
    const socket = new WebSocket(`${proto}//${window.location.host}/api/ws`)
    this.socket = socket

    socket.onopen = () => {
      this.reconnectDelay = 1000
      // Re-issue watches so a reconnect resumes the same live view.
      for (const [connectionId, filter] of this.watches) {
        this.send({ type: 'watch', connectionId, filter })
      }
      this.emit({ type: 'hello' })
    }

    socket.onmessage = (event) => {
      try {
        this.emit(JSON.parse(event.data as string) as Frame)
      } catch {
        // A frame we cannot parse is not worth tearing the socket down for.
      }
    }

    socket.onclose = () => {
      this.socket = null
      if (this.closedByUs) return
      // Back off up to 30s so a server restart does not spin the browser.
      this.reconnectTimer = window.setTimeout(() => this.connect(), this.reconnectDelay)
      this.reconnectDelay = Math.min(this.reconnectDelay * 2, 30000)
    }
  }

  disconnect() {
    this.closedByUs = true
    if (this.reconnectTimer !== null) {
      window.clearTimeout(this.reconnectTimer)
      this.reconnectTimer = null
    }
    this.socket?.close()
    this.socket = null
    this.watches.clear()
  }

  private send(payload: unknown) {
    if (this.socket?.readyState === WebSocket.OPEN) {
      this.socket.send(JSON.stringify(payload))
    }
  }

  watch(connectionId: string, filter = '') {
    this.watches.set(connectionId, filter)
    this.send({ type: 'watch', connectionId, filter })
  }

  unwatch(connectionId: string) {
    this.watches.delete(connectionId)
    this.send({ type: 'unwatch', connectionId })
  }

  subscribe(listener: Listener): () => void {
    this.listeners.add(listener)
    return () => this.listeners.delete(listener)
  }

  private emit(frame: Frame) {
    for (const listener of this.listeners) listener(frame)
  }
}

export const liveSocket = new LiveSocket()

/** useFrames subscribes to every frame the socket receives. */
export function useFrames(listener: Listener) {
  const ref = useRef(listener)
  ref.current = listener

  useEffect(() => liveSocket.subscribe((frame) => ref.current(frame)), [])
}

/**
 * useLiveMessages streams messages for one connection, keeping the newest
 * `limit` in memory. Paused streams stop appending but keep what is on screen.
 */
export function useLiveMessages(connectionId: string | undefined, filter: string, limit = 500) {
  const [messages, setMessages] = useState<Message[]>([])
  const [paused, setPaused] = useState(false)
  const pausedRef = useRef(paused)
  pausedRef.current = paused

  useEffect(() => {
    if (!connectionId) return
    setMessages([])
    liveSocket.watch(connectionId, filter)
    return () => liveSocket.unwatch(connectionId)
  }, [connectionId, filter])

  useFrames((frame) => {
    if (frame.type !== 'message' || pausedRef.current) return
    const msg = frame.data as Message
    if (msg.connectionId !== connectionId) return
    setMessages((prev) => {
      const next = [msg, ...prev]
      return next.length > limit ? next.slice(0, limit) : next
    })
  })

  return { messages, paused, setPaused, clear: () => setMessages([]) }
}

/** useConnectionStatus keeps a live map of connection id to status. */
export function useConnectionStatus() {
  const [statuses, setStatuses] = useState<Record<string, Status>>({})

  useFrames((frame) => {
    if (frame.type !== 'status') return
    const status = frame.data as Status
    setStatuses((prev) => ({ ...prev, [status.connectionId]: status }))
  })

  return statuses
}
