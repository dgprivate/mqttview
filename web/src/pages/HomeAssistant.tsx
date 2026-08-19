import { useCallback, useEffect, useState } from 'react'
import { Link } from 'react-router-dom'
import { api } from '../api/client'
import type { Connection, HassDevice, HassEntity, HassStatus } from '../api/types'
import { useAuth } from '../auth/AuthContext'
import { Alert, Empty, formatRelative, Spinner } from '../components/common'
import { useFrames } from '../ws/socket'

/**
 * HomeAssistant renders devices discovered through the Home Assistant MQTT
 * discovery standard, and lets an operator act on the ones that expose a
 * command topic.
 */
export function HomeAssistant() {
  const { can } = useAuth()
  const [status, setStatus] = useState<HassStatus | null>(null)
  const [devices, setDevices] = useState<HassDevice[] | null>(null)
  const [connections, setConnections] = useState<Connection[]>([])
  const [connectionId, setConnectionId] = useState('')
  const [search, setSearch] = useState('')
  const [error, setError] = useState('')
  const [disabled, setDisabled] = useState(false)

  const load = useCallback(async () => {
    try {
      const [s, d] = await Promise.all([api.hassStatus(), api.hassDevices(connectionId)])
      setStatus(s)
      setDevices(d)
      setDisabled(false)
      setError('')
    } catch (err) {
      const message = err instanceof Error ? err.message : 'Could not load devices'
      // A disabled plugin is a normal state, not an error to shout about.
      if (message.includes('disabled')) {
        setDisabled(true)
        setDevices([])
      } else {
        setError(message)
      }
    }
  }, [connectionId])

  useEffect(() => {
    api.connections().then(setConnections).catch(() => undefined)
  }, [])

  useEffect(() => {
    void load()
  }, [load])

  // The plugin batches its change events, so reloading on each one is cheap.
  useFrames((frame) => {
    if (frame.type === 'event' && frame.event === 'plugin:home-assistant:changed') {
      void load()
    }
  })

  if (disabled) {
    return (
      <Empty>
        The Home Assistant plugin is switched off.{' '}
        {can('admin') ? <Link to="/plugins">Enable it on the plugins page</Link> : 'Ask an administrator to enable it.'}
      </Empty>
    )
  }

  if (!devices) return error ? <Alert kind="error">{error}</Alert> : <Spinner />

  const needle = search.trim().toLowerCase()
  const visible = needle
    ? devices
        .map((d) => ({
          ...d,
          entities: d.entities.filter(
            (e) =>
              e.name.toLowerCase().includes(needle) ||
              e.component.toLowerCase().includes(needle) ||
              (e.stateTopic ?? '').toLowerCase().includes(needle),
          ),
        }))
        .filter((d) => d.name.toLowerCase().includes(needle) || d.entities.length > 0)
    : devices

  return (
    <>
      <div className="page-head">
        <div>
          <h1>Home Assistant devices</h1>
          <p className="subtitle">
            Discovered under <span className="mono">{status?.discoveryPrefix ?? 'homeassistant'}/</span> ·{' '}
            {status?.devices ?? 0} devices · {status?.entities ?? 0} entities
            {status && !status.allowControl && ' · control disabled in settings'}
          </p>
        </div>
      </div>

      <Alert kind="error">{error}</Alert>

      <div className="card">
        <div className="field-row two">
          <div className="field">
            <label htmlFor="hass-conn">Broker</label>
            <select id="hass-conn" value={connectionId} onChange={(e) => setConnectionId(e.target.value)}>
              <option value="">All brokers</option>
              {connections.map((c) => (
                <option key={c.id} value={c.id}>
                  {c.name}
                </option>
              ))}
            </select>
          </div>
          <div className="field">
            <label htmlFor="hass-search">Search</label>
            <input
              id="hass-search"
              value={search}
              onChange={(e) => setSearch(e.target.value)}
              placeholder="Kitchen, sensor, temperature…"
            />
          </div>
        </div>
      </div>

      {visible.length === 0 ? (
        <Empty>
          No devices discovered yet. Connect to a broker where devices publish Home Assistant
          discovery messages.
        </Empty>
      ) : (
        visible.map((device) => (
          <DeviceCard
            key={`${device.connectionId}-${device.key}`}
            device={device}
            controllable={can('operator') && (status?.allowControl ?? false)}
            onChanged={load}
          />
        ))
      )}
    </>
  )
}

function DeviceCard({
  device,
  controllable,
  onChanged,
}: {
  device: HassDevice
  controllable: boolean
  onChanged: () => void
}) {
  const meta = [device.manufacturer, device.model, device.swVersion && `v${device.swVersion}`, device.origin]
    .filter(Boolean)
    .join(' · ')

  return (
    <div className="device">
      <div className="device-head">
        <h3>{device.name}</h3>
        {device.suggestedArea && <span className="badge">{device.suggestedArea}</span>}
        <span className="badge">{device.entities.length} entities</span>
        {device.key !== '__ungrouped__' && (
          <button
            className="small"
            title={device.pinned ? 'Unpin' : 'Pin to the top'}
            onClick={async () => {
              await api.hassPin(device.key, device.connectionId, !device.pinned)
              onChanged()
            }}
          >
            {device.pinned ? '★' : '☆'}
          </button>
        )}
        {device.configurationUrl && (
          <a className="btn small" href={device.configurationUrl} target="_blank" rel="noreferrer">
            Configure
          </a>
        )}
        {meta && <div className="device-meta">{meta}</div>}
      </div>

      {device.entities.map((entity) => (
        <EntityRow key={entity.id} entity={entity} controllable={controllable} />
      ))}
    </div>
  )
}

function EntityRow({ entity, controllable }: { entity: HassEntity; controllable: boolean }) {
  const [error, setError] = useState('')
  const [busy, setBusy] = useState(false)
  const [input, setInput] = useState('')

  async function run(action: string, value = '') {
    setError('')
    setBusy(true)
    try {
      await api.hassCommand(entity.id, action, value)
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Command failed')
    } finally {
      setBusy(false)
    }
  }

  const actions = entity.actions ?? []
  // Actions that need a value get an input; the rest are plain buttons.
  const valueAction = actions.find((a) =>
    ['set', 'set_brightness', 'set_percentage', 'set_position', 'set_temperature', 'set_humidity'].includes(a),
  )
  const simpleActions = actions.filter((a) => a !== valueAction && a !== 'publish')

  return (
    <div className="entity">
      <div className="entity-name">
        {entity.name}
        <small>
          {entity.component}
          {entity.deviceClass ? ` · ${entity.deviceClass}` : ''}
          {entity.availability === 'offline' ? ' · offline' : ''}
          {entity.state ? ` · ${formatRelative(entity.state.updatedAt)}` : ''}
        </small>
        {error && <small style={{ color: 'var(--err)' }}>{error}</small>}
      </div>

      <div className="entity-value">
        {entity.availability === 'offline' ? (
          <span className="badge err">offline</span>
        ) : entity.state ? (
          <span title={entity.state.templateSupported ? undefined : 'Raw payload: mqttview cannot evaluate this value_template'}>
            {formatValue(entity.state.value)}
            {entity.unit ? ` ${entity.unit}` : ''}
            {!entity.state.templateSupported && ' *'}
          </span>
        ) : (
          <span className="badge">no value</span>
        )}
      </div>

      {controllable && actions.length > 0 && (
        <div className="entity-actions">
          {simpleActions.map((action) => (
            <button key={action} className="small" disabled={busy} onClick={() => void run(action)}>
              {action.replace(/_/g, ' ')}
            </button>
          ))}

          {valueAction && (
            <>
              <input
                aria-label={`${valueAction} value for ${entity.name}`}
                style={{ maxWidth: '8rem', minHeight: '34px' }}
                value={input}
                inputMode={valueAction === 'set' && entity.component === 'text' ? 'text' : 'decimal'}
                onChange={(e) => setInput(e.target.value)}
                placeholder="value"
              />
              <button className="small primary" disabled={busy} onClick={() => void run(valueAction, input)}>
                {valueAction.replace(/_/g, ' ')}
              </button>
            </>
          )}
        </div>
      )}
    </div>
  )
}

function formatValue(value: unknown): string {
  if (value === null || value === undefined) return '—'
  if (typeof value === 'object') return JSON.stringify(value)
  return String(value)
}
