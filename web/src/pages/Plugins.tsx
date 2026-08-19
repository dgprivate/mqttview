import { useCallback, useEffect, useState } from 'react'
import { api } from '../api/client'
import type { PluginInfo, SettingField } from '../api/types'
import { useAuth } from '../auth/AuthContext'
import { Alert, Empty, Spinner } from '../components/common'

export function Plugins() {
  const { can } = useAuth()
  const [plugins, setPlugins] = useState<PluginInfo[] | null>(null)
  const [error, setError] = useState('')

  const load = useCallback(async () => {
    try {
      setPlugins(await api.plugins())
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Could not load plugins')
    }
  }, [])

  useEffect(() => {
    void load()
  }, [load])

  if (!plugins) return error ? <Alert kind="error">{error}</Alert> : <Spinner />

  return (
    <>
      <div className="page-head">
        <div>
          <h1>Plugins</h1>
          <p className="subtitle">
            Plugins observe MQTT traffic and add their own views. They are compiled into the server.
          </p>
        </div>
      </div>

      <Alert kind="error">{error}</Alert>

      {plugins.length === 0 ? (
        <Empty>No plugins are compiled into this build.</Empty>
      ) : (
        plugins.map((p) => (
          <PluginCard key={p.meta.id} plugin={p} editable={can('admin')} onChanged={load} />
        ))
      )}
    </>
  )
}

function PluginCard({
  plugin,
  editable,
  onChanged,
}: {
  plugin: PluginInfo
  editable: boolean
  onChanged: () => void
}) {
  const [settings, setSettings] = useState<Record<string, unknown>>(plugin.settings ?? {})
  const [status, setStatus] = useState<{ kind: 'error' | 'ok'; text: string } | null>(null)
  const [busy, setBusy] = useState(false)

  useEffect(() => {
    setSettings(plugin.settings ?? {})
  }, [plugin.settings])

  async function toggle() {
    setStatus(null)
    setBusy(true)
    try {
      await api.setPluginEnabled(plugin.meta.id, !plugin.enabled)
      onChanged()
    } catch (err) {
      setStatus({ kind: 'error', text: err instanceof Error ? err.message : 'Could not change the plugin' })
    } finally {
      setBusy(false)
    }
  }

  async function save() {
    setStatus(null)
    setBusy(true)
    try {
      await api.savePluginSettings(plugin.meta.id, settings)
      setStatus({ kind: 'ok', text: 'Settings saved.' })
      onChanged()
    } catch (err) {
      setStatus({ kind: 'error', text: err instanceof Error ? err.message : 'Could not save settings' })
    } finally {
      setBusy(false)
    }
  }

  const schema = plugin.meta.settingsSchema ?? []

  return (
    <div className="card">
      <div className="card-head">
        <div>
          <h2>
            {plugin.meta.name}{' '}
            <span className="badge">{plugin.meta.version}</span>{' '}
            {plugin.enabled ? (
              <span className="badge ok">enabled</span>
            ) : (
              <span className="badge">disabled</span>
            )}
          </h2>
          <p className="subtitle">{plugin.meta.description}</p>
        </div>
        {editable && (
          <button className={plugin.enabled ? '' : 'primary'} disabled={busy} onClick={() => void toggle()}>
            {plugin.enabled ? 'Disable' : 'Enable'}
          </button>
        )}
      </div>

      {plugin.error && <Alert kind="error">{plugin.error}</Alert>}
      {plugin.dropped > 0 && (
        <Alert kind="info">
          {plugin.dropped.toLocaleString()} messages were dropped because the plugin could not keep
          up.
        </Alert>
      )}
      {status && <Alert kind={status.kind}>{status.text}</Alert>}

      {plugin.meta.url && (
        <p className="subtitle">
          <a href={plugin.meta.url} target="_blank" rel="noreferrer">
            Documentation
          </a>
        </p>
      )}

      {editable && schema.length > 0 && (
        <>
          <h2 style={{ marginTop: '1rem' }}>Settings</h2>
          {schema.map((field) => (
            <SettingInput
              key={field.key}
              field={field}
              value={settings[field.key]}
              onChange={(value) => setSettings((prev) => ({ ...prev, [field.key]: value }))}
            />
          ))}
          <button className="primary" disabled={busy} onClick={() => void save()}>
            {busy ? <span className="spinner" /> : 'Save settings'}
          </button>
        </>
      )}
    </div>
  )
}

function SettingInput({
  field,
  value,
  onChange,
}: {
  field: SettingField
  value: unknown
  onChange: (value: unknown) => void
}) {
  const id = `setting-${field.key}`

  if (field.type === 'bool') {
    return (
      <div className="field">
        <div className="checkbox">
          <input
            id={id}
            type="checkbox"
            checked={Boolean(value ?? field.default)}
            onChange={(e) => onChange(e.target.checked)}
          />
          <label htmlFor={id}>{field.label}</label>
        </div>
        {field.description && <p className="subtitle">{field.description}</p>}
      </div>
    )
  }

  return (
    <div className="field">
      <label htmlFor={id}>{field.label}</label>
      {field.type === 'select' ? (
        <select id={id} value={String(value ?? field.default ?? '')} onChange={(e) => onChange(e.target.value)}>
          {(field.options ?? []).map((o) => (
            <option key={o.value} value={o.value}>
              {o.label}
            </option>
          ))}
        </select>
      ) : (
        <input
          id={id}
          type={field.type === 'number' ? 'number' : 'text'}
          inputMode={field.type === 'number' ? 'numeric' : undefined}
          value={String(value ?? field.default ?? '')}
          onChange={(e) => onChange(field.type === 'number' ? Number(e.target.value) : e.target.value)}
        />
      )}
      {field.description && <p className="subtitle">{field.description}</p>}
    </div>
  )
}
