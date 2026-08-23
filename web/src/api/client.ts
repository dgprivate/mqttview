import { apiURL } from './base'
import type {
  AuthConfig,
  Health,
  Connection,
  ConnectionInput,
  HassDevice,
  HassStatus,
  Message,
  PlcCommands,
  PlcEdge,
  PlcMapping,
  PlcState,
  PlcStatus,
  PluginInfo,
  Role,
  TwoFactorStatus,
  TopicValue,
  TreeNode,
  User,
} from './types'

/** ApiError carries the server's message so the UI can show it verbatim. */
export class ApiError extends Error {
  readonly status: number
  /**
   * True when the server refused because it wants a second factor, not because
   * the credentials were wrong. The login form needs to tell those apart.
   */
  readonly twoFactorRequired: boolean

  constructor(status: number, message: string, twoFactorRequired = false) {
    super(message)
    this.name = 'ApiError'
    this.status = status
    this.twoFactorRequired = twoFactorRequired
  }
}

function csrfToken(): string {
  const match = document.cookie.match(/(?:^|;\s*)mqttview_csrf=([^;]*)/)
  return match ? decodeURIComponent(match[1]) : ''
}

async function request<T>(path: string, init: RequestInit = {}): Promise<T> {
  const headers = new Headers(init.headers)
  if (init.body !== undefined) {
    headers.set('Content-Type', 'application/json')
  }
  // The double-submit token proves the request came from our own page.
  const token = csrfToken()
  if (token) {
    headers.set('X-CSRF-Token', token)
  }

  // apiURL rather than the path as written: under Home Assistant ingress the
  // UI lives below a prefix, and an absolute path would leave the panel.
  const response = await fetch(apiURL(path), { ...init, headers, credentials: 'same-origin' })

  if (response.status === 204) {
    return undefined as T
  }

  const text = await response.text()
  let body: unknown = null
  if (text) {
    try {
      body = JSON.parse(text)
    } catch {
      body = { error: text }
    }
  }

  if (!response.ok) {
    const message =
      body && typeof body === 'object' && 'error' in body
        ? String((body as { error: unknown }).error)
        : `Request failed with status ${response.status}`
    const twoFactor =
      body !== null && typeof body === 'object' && (body as { twoFactorRequired?: boolean }).twoFactorRequired === true
    throw new ApiError(response.status, message, twoFactor)
  }
  return body as T
}

/** decodePayload turns a base64 payload into bytes. */
export function decodePayload(base64: string | null | undefined): Uint8Array {
  if (!base64) return new Uint8Array()
  const binary = atob(base64)
  const bytes = new Uint8Array(binary.length)
  for (let i = 0; i < binary.length; i++) {
    bytes[i] = binary.charCodeAt(i)
  }
  return bytes
}

/** payloadToText decodes a payload as UTF-8, falling back to a hex dump. */
export function payloadToText(base64: string | null | undefined): string {
  const bytes = decodePayload(base64)
  if (bytes.length === 0) return ''
  try {
    return new TextDecoder('utf-8', { fatal: true }).decode(bytes)
  } catch {
    return Array.from(bytes)
      .map((b) => b.toString(16).padStart(2, '0'))
      .join(' ')
  }
}

/** looksBinary reports whether a payload failed to decode as UTF-8 text. */
export function looksBinary(base64: string | null | undefined): boolean {
  const bytes = decodePayload(base64)
  if (bytes.length === 0) return false
  try {
    new TextDecoder('utf-8', { fatal: true }).decode(bytes)
    return false
  } catch {
    return true
  }
}

export const api = {
  /** health is public: it is what a container probe calls, and it carries the
   *  running version, which is the only way to tell what is actually live. */
  health: () => request<Health>('/api/health'),

  // --- auth ---
  authConfig: () => request<AuthConfig>('/api/auth/config'),
  /** login sends the code only when the server has asked for one. */
  login: (email: string, password: string, code = '') =>
    request<User>('/api/auth/login', { method: 'POST', body: JSON.stringify({ email, password, code }) }),

  // --- two-factor ---
  twoFactorStatus: () => request<TwoFactorStatus>('/api/auth/2fa'),
  twoFactorEnrol: () => request<{ secret: string; uri: string }>('/api/auth/2fa/enrol', { method: 'POST' }),
  twoFactorConfirm: (code: string) =>
    request<{ recoveryCodes: string[] }>('/api/auth/2fa/confirm', {
      method: 'POST',
      body: JSON.stringify({ code }),
    }),
  twoFactorDisable: (password: string, code: string) =>
    request<void>('/api/auth/2fa/disable', { method: 'POST', body: JSON.stringify({ password, code }) }),
  regenerateRecoveryCodes: (code: string) =>
    request<{ recoveryCodes: string[] }>('/api/auth/2fa/recovery-codes', {
      method: 'POST',
      body: JSON.stringify({ code }),
    }),
  clearUserTwoFactor: (id: string) => request<void>(`/api/users/${id}/2fa`, { method: 'DELETE' }),
  logout: () => request<void>('/api/auth/logout', { method: 'POST' }),
  me: () => request<User>('/api/auth/me'),
  changePassword: (currentPassword: string, newPassword: string) =>
    request<void>('/api/auth/password', {
      method: 'POST',
      body: JSON.stringify({ currentPassword, newPassword }),
    }),

  // --- connections ---
  connections: () => request<Connection[]>('/api/connections'),
  connection: (id: string) => request<Connection>(`/api/connections/${id}`),
  createConnection: (input: ConnectionInput) =>
    request<Connection>('/api/connections', { method: 'POST', body: JSON.stringify(input) }),
  updateConnection: (id: string, input: ConnectionInput) =>
    request<Connection>(`/api/connections/${id}`, { method: 'PUT', body: JSON.stringify(input) }),
  deleteConnection: (id: string) => request<void>(`/api/connections/${id}`, { method: 'DELETE' }),
  connect: (id: string) => request<Connection>(`/api/connections/${id}/connect`, { method: 'POST' }),
  disconnect: (id: string) =>
    request<Connection>(`/api/connections/${id}/disconnect`, { method: 'POST' }),
  clearState: (id: string) => request<Connection>(`/api/connections/${id}/clear`, { method: 'POST' }),

  publish: (
    id: string,
    body: { topic: string; payload: string; payloadBase64?: boolean; qos: number; retain: boolean },
  ) => request<void>(`/api/connections/${id}/publish`, { method: 'POST', body: JSON.stringify(body) }),

  subscribe: (id: string, subscriptions: { filter: string; qos: number }[]) =>
    request<Connection>(`/api/connections/${id}/subscribe`, {
      method: 'POST',
      body: JSON.stringify({ subscriptions }),
    }),
  unsubscribe: (id: string, filters: string[]) =>
    request<Connection>(`/api/connections/${id}/unsubscribe`, {
      method: 'POST',
      body: JSON.stringify({ filters }),
    }),

  tree: (id: string, prefix: string) =>
    request<{ prefix: string; children: TreeNode[] }>(
      `/api/connections/${id}/tree?prefix=${encodeURIComponent(prefix)}`,
    ),
  topic: (id: string, topic: string) =>
    request<TreeNode>(`/api/connections/${id}/topic?topic=${encodeURIComponent(topic)}`),
  messages: (id: string, limit = 200, filter = '') =>
    request<Message[]>(
      `/api/connections/${id}/messages?limit=${limit}&filter=${encodeURIComponent(filter)}`,
    ),
  search: (id: string, q: string) =>
    request<TopicValue[]>(`/api/connections/${id}/search?q=${encodeURIComponent(q)}`),

  // --- users ---
  users: () => request<User[]>('/api/users'),
  createUser: (body: { email: string; name: string; role: Role; password: string }) =>
    request<User>('/api/users', { method: 'POST', body: JSON.stringify(body) }),
  updateUser: (id: string, body: { email: string; name: string; role: Role; disabled: boolean }) =>
    request<User>(`/api/users/${id}`, { method: 'PUT', body: JSON.stringify(body) }),
  resetPassword: (id: string, password: string) =>
    request<void>(`/api/users/${id}/password`, { method: 'PUT', body: JSON.stringify({ password }) }),
  deleteUser: (id: string) => request<void>(`/api/users/${id}`, { method: 'DELETE' }),

  // --- plugins ---
  plugins: () => request<PluginInfo[]>('/api/plugins'),
  setPluginEnabled: (id: string, enabled: boolean) =>
    request<PluginInfo>(`/api/plugins/${id}/enabled`, {
      method: 'PUT',
      body: JSON.stringify({ enabled }),
    }),
  savePluginSettings: (id: string, settings: Record<string, unknown>) =>
    request<PluginInfo>(`/api/plugins/${id}/settings`, {
      method: 'PUT',
      body: JSON.stringify(settings),
    }),

  // --- home assistant plugin ---
  hassStatus: () => request<HassStatus>('/api/p/home-assistant/status'),
  hassDevices: (connectionId = '') =>
    request<HassDevice[]>(
      `/api/p/home-assistant/devices${connectionId ? `?connectionId=${encodeURIComponent(connectionId)}` : ''}`,
    ),
  /**
   * The entity ID and the device key both contain the discovery topic, slashes
   * and all, so they travel in the body: a path parameter cannot hold them and
   * percent-encoding does not survive Go's path decoding.
   */
  hassCommand: (entityId: string, action: string, value = '') =>
    request<{ topic: string; payload: string }>('/api/p/home-assistant/command', {
      method: 'POST',
      body: JSON.stringify({ entityId, action, value }),
    }),
  hassPin: (deviceKey: string, connectionId: string, pinned: boolean) =>
    request<{ pinned: boolean }>('/api/p/home-assistant/pin', {
      method: 'POST',
      body: JSON.stringify({ deviceKey, connectionId, pinned }),
    }),

  // --- beckhoff plc plugin ---
  plcStatus: () => request<PlcStatus>('/api/p/beckhoff-plc/status'),
  plcState: (connectionId = '') =>
    request<PlcState>(
      `/api/p/beckhoff-plc/state${connectionId ? `?connectionId=${encodeURIComponent(connectionId)}` : ''}`,
    ),
  /**
   * plcEdges reads the digital signal journal. Passing `since` returns only
   * what is newer, so the discovery view can append rather than reload.
   */
  plcEdges: (opts: { connectionId?: string; since?: number; limit?: number; rising?: boolean } = {}) => {
    const q = new URLSearchParams()
    if (opts.connectionId) q.set('connectionId', opts.connectionId)
    if (opts.since) q.set('since', String(opts.since))
    if (opts.limit) q.set('limit', String(opts.limit))
    if (opts.rising) q.set('rising', 'true')
    const suffix = q.toString() ? `?${q}` : ''
    return request<{ edges: PlcEdge[]; seq: number }>(`/api/p/beckhoff-plc/edges${suffix}`)
  },
  plcCommands: () => request<PlcCommands>('/api/p/beckhoff-plc/commands'),
  plcCommand: (body: {
    connectionId?: string
    target: string
    command: string
    address?: number
    params?: string[]
  }) =>
    request<{ topic: string; payload: string }>('/api/p/beckhoff-plc/command', {
      method: 'POST',
      body: JSON.stringify(body),
    }),
  plcMappings: (connectionId = '') =>
    request<PlcMapping[]>(
      `/api/p/beckhoff-plc/mappings${connectionId ? `?connectionId=${encodeURIComponent(connectionId)}` : ''}`,
    ),
  plcSetMapping: (body: PlcMapping & { connectionId?: string }) =>
    request<PlcMapping>('/api/p/beckhoff-plc/mappings', {
      method: 'PUT',
      body: JSON.stringify(body),
    }),
}
