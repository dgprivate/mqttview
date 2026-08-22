// Shapes returned by the mqttview API. They mirror the Go structs; payloads
// are base64 because MQTT messages are arbitrary bytes, not text.

export type Role = 'admin' | 'operator' | 'viewer'

export interface User {
  id: string
  email: string
  name: string
  role: Role
  provider: string
  disabled: boolean
  createdAt: string
  lastLoginAt?: string
}

export interface ProviderInfo {
  id: string
  displayName: string
}

export interface AuthConfig {
  allowLocal: boolean
  providers: ProviderInfo[]
  needsBootstrap: boolean
}

export type ConnectionState = 'disconnected' | 'connecting' | 'connected' | 'error'

export interface Status {
  connectionId: string
  state: ConnectionState
  since: string
  connectedAt?: string
  lastError?: string
  attempts: number
  received: number
  sent: number
  sessionPresent: boolean
  version: string
}

export interface Subscription {
  filter: string
  qos: number
  noLocal?: boolean
  retainAsPublished?: boolean
  retainHandling?: number
}

export interface Will {
  topic: string
  payload: string
  qos: number
  retain: boolean
  delayInterval?: number
}

export interface TlsView {
  insecureSkipVerify: boolean
  serverName?: string
  minVersion?: string
  alpn?: string[]
  hasCa: boolean
  hasClientCert: boolean
}

export interface Connection {
  id: string
  name: string
  url: string
  version: string
  clientId: string
  username: string
  hasPassword: boolean
  keepAlive: number
  cleanStart: boolean
  sessionExpiry: number
  connectTimeout: number
  tls: TlsView
  will?: Will
  subscriptions: Subscription[]
  autoConnect: boolean
  historySize: number
  status: Status
  topics: number
  treeFull: boolean
}

/** ConnectionInput is the write shape. Omitted secrets keep their stored value. */
export interface ConnectionInput {
  name: string
  url: string
  version: string
  clientId: string
  username: string
  password?: string | null
  keepAlive: number
  cleanStart: boolean
  sessionExpiry: number
  tls: {
    insecureSkipVerify: boolean
    serverName: string
    minVersion: string
    alpn: string[] | null
    caPem?: string | null
    clientCertPem?: string | null
    clientKeyPem?: string | null
  }
  will: Will | null
  subscriptions: Subscription[]
  autoConnect: boolean
  historySize: number
}

export interface MessageProps {
  contentType?: string
  responseTopic?: string
  correlationData?: string
  payloadFormat?: number
  messageExpiry?: number
  user?: Record<string, string>
}

export interface Message {
  connectionId: string
  topic: string
  /** base64-encoded payload */
  payload: string
  qos: number
  retain: boolean
  duplicate?: boolean
  receivedAt: string
  seq: number
  props?: MessageProps
}

export interface TopicValue {
  topic: string
  /** base64-encoded payload */
  payload: string
  truncated?: boolean
  size: number
  qos: number
  retain: boolean
  updatedAt: string
  count: number
}

export interface TreeNode {
  name: string
  topic: string
  childCount: number
  topicCount: number
  messages: number
  value?: TopicValue
}

export interface SettingOption {
  value: string
  label: string
}

export interface SettingField {
  key: string
  label: string
  type: 'string' | 'bool' | 'number' | 'select'
  default?: unknown
  description?: string
  options?: SettingOption[]
}

export interface PluginMeta {
  id: string
  name: string
  version: string
  description: string
  author?: string
  url?: string
  panel?: string
  settingsSchema?: SettingField[]
}

export interface PluginInfo {
  meta: PluginMeta
  enabled: boolean
  error?: string
  settings: Record<string, unknown>
  dropped: number
}

// --- Home Assistant plugin ---

export type Availability = 'unknown' | 'online' | 'offline'

export interface EntityState {
  raw: string
  value: unknown
  templateSupported: boolean
  updatedAt: string
}

export interface HassEntity {
  id: string
  connectionId: string
  discoveryTopic: string
  component: string
  nodeId?: string
  objectId: string
  uniqueId?: string
  name: string
  deviceKey: string
  deviceClass?: string
  stateClass?: string
  unit?: string
  icon?: string
  category?: string
  stateTopic?: string
  commandTopic?: string
  controllable: boolean
  actions?: string[]
  availability: Availability
  state?: EntityState
  attributes?: Record<string, unknown>
  config: Record<string, unknown>
  discoveredAt: string
  updatedAt: string
}

export interface HassDevice {
  key: string
  connectionId: string
  name: string
  manufacturer?: string
  model?: string
  modelId?: string
  swVersion?: string
  hwVersion?: string
  serialNumber?: string
  suggestedArea?: string
  configurationUrl?: string
  identifiers?: string[]
  connections?: string[][]
  viaDevice?: string
  origin?: string
  pinned: boolean
  firstSeen: string
  lastSeen: string
  entities: HassEntity[]
}

export interface HassStatus {
  discoveryPrefix: string
  devices: number
  entities: number
  allowControl: boolean
}

// --- beckhoff plc plugin ---

export type PlcKind = 'input' | 'output' | 'temperature'

export interface PlcPoint {
  connectionId: string
  topic: string
  kind: PlcKind
  address: number
  name: string
  device?: string
  bool?: boolean
  number?: number
  updatedAt: string
  label?: string
  location?: string
  sensorType?: string
  alarmZone?: boolean
  state?: string
}

export interface PlcLight {
  connectionId: string
  topic: string
  address: number
  name: string
  status: number
  actualLevel: number
  minLevel: number
  maxLevel: number
  fadeTime: number
  fadeRate: number
  lastCommand?: string
  error?: string
  updatedAt: string
}

export interface PlcShade {
  topic: string
  slug: string
  position: number
  lastCommand?: string
  updatedAt: string
}

export interface PlcActuator {
  topic: string
  group: string
  slug: string
  state?: string
  updatedAt: string
}

export interface PlcElectricity {
  name: string
  phases: { voltage: number; current: number }[]
  frequency: number
  voltageImbalance: number
  currentImbalance: number
  alarmActive: boolean
  activeAlarms?: string[]
  updatedAt: string
}

export interface PlcMeter {
  name: string
  available: boolean
  readings?: Record<string, number>
  /** Keyed like readings; a missing key means the meter publishes no unit. */
  units?: Record<string, string>
  updatedAt: string
}

export interface PlcWatchdog {
  uptimeS: number
  alive: boolean
  mqttConnected: boolean
  backupMqttConnected: boolean
  fallbackActive: boolean
  alarmMode: string
  alarmTriggered: boolean
  ready: boolean
  persistentValid: boolean
  streams?: Record<string, { count: number; ageS: number }>
  updatedAt: string
}

export interface PlcSummary {
  inputs: number
  outputs: number
  temperatures: number
  lights: number
  lightsWithError: number
  lightsOn: number
  shades: number
  actuators: number
  meters: number
  described: number
  activeInputs: number
}

export interface PlcState {
  points: PlcPoint[]
  lights: PlcLight[]
  shades: PlcShade[]
  actuators: PlcActuator[]
  electricity: PlcElectricity[]
  meters: PlcMeter[]
  watchdog?: PlcWatchdog
  bridge?: { source?: string; version?: string; uptimeHours: number; rssMb: number; freeMemMb: number }
  summary: PlcSummary
}

export interface PlcEdge {
  seq: number
  connectionId: string
  topic: string
  kind: PlcKind
  address: number
  name: string
  label?: string
  location?: string
  sensorType?: string
  alarmZone?: boolean
  from: boolean
  to: boolean
  at: string
}

export interface PlcStatus {
  topicPrefix: string
  meterPrefix: string
  points: number
  lights: number
  edges: number
  seq: number
  readOnly: boolean
}

export interface PlcCommandSpec {
  target: string
  command: string
  label: string
  description: string
  minAddress?: number
  maxAddress?: number
  param?: string
  tier: 'light' | 'output'
}

export interface PlcCommands {
  commands: PlcCommandSpec[]
  allowControl: boolean
  allowDigitalOutputs: boolean
  commandTopic: string
}

export interface PlcMapping {
  name: string
  label?: string
  location?: string
  type?: string
  notes?: string
}
