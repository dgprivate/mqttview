import { useEffect, useState } from 'react'
import { BrowserRouter, Navigate, NavLink, Route, Routes, useLocation } from 'react-router-dom'
import { basePath } from './api/base'
import { api } from './api/client'
import type { PluginInfo } from './api/types'
import { AuthProvider, useAuth } from './auth/AuthContext'
import { Spinner } from './components/common'
import { Account } from './pages/Account'
import { BeckhoffPlc } from './pages/BeckhoffPlc'
import { ConnectionForm } from './pages/ConnectionForm'
import { Connections } from './pages/Connections'
import { Explorer } from './pages/Explorer'
import { HomeAssistant } from './pages/HomeAssistant'
import { Login } from './pages/Login'
import { Plugins } from './pages/Plugins'
import { Users } from './pages/Users'

export default function App() {
  return (
    <BrowserRouter basename={basePath()}>
      <AuthProvider>
        <Shell />
      </AuthProvider>
    </BrowserRouter>
  )
}

function Shell() {
  const { user, loading, mode } = useAuth()

  if (loading) return <Spinner label="Starting mqttview…" />
  // No user in Home Assistant mode is not "please sign in": there is nothing
  // to sign in with. It means the request did not reach us through Home
  // Assistant, and a login form would send somebody looking for a password
  // that was never created.
  if (!user && mode === 'ingress') return <IngressBlocked />
  if (!user) return <Login />

  return (
    <div className="app">
      <Navigation />
      <main>
        <Routes>
          <Route path="/" element={<Navigate to="/connections" replace />} />
          <Route path="/login" element={<Navigate to="/connections" replace />} />
          <Route path="/connections" element={<Connections />} />
          <Route path="/connections/new" element={<ConnectionForm />} />
          <Route path="/connections/:id" element={<Explorer />} />
          <Route path="/connections/:id/edit" element={<ConnectionForm />} />
          <Route path="/plugins" element={<Plugins />} />
          <Route path="/home-assistant" element={<HomeAssistant />} />
          <Route path="/beckhoff-plc" element={<BeckhoffPlc />} />
          <Route path="/users" element={<Users />} />
          <Route path="/account" element={<Account />} />
          <Route path="*" element={<Navigate to="/connections" replace />} />
        </Routes>
      </main>
    </div>
  )
}

/**
 * IngressBlocked is what shows when Home Assistant mode is on but the request
 * did not come through Home Assistant. It is nearly always one thing: the
 * add-on port was published and opened directly.
 */
function IngressBlocked() {
  return (
    <div className="app">
      <main>
        <div className="card" style={{ maxWidth: '34rem', margin: '3rem auto' }}>
          <h2>Open this from Home Assistant</h2>
          <p>
            mqttview is running in Home Assistant mode, so it only accepts requests that come
            through Home Assistant, which is what decides who may see this panel.
          </p>
          <p>
            Use the mqttview entry in the Home Assistant sidebar. If you reached this page at a
            host and port directly, that port does not need to be published at all — ingress
            does not use it.
          </p>
        </div>
      </main>
    </div>
  )
}

function Navigation() {
  const { user, signOut, can, mode } = useAuth()
  const [open, setOpen] = useState(false)
  const [plugins, setPlugins] = useState<PluginInfo[]>([])
  const location = useLocation()

  useEffect(() => {
    api.plugins().then(setPlugins).catch(() => undefined)
  }, [])

  // Close the mobile menu whenever the route changes.
  useEffect(() => setOpen(false), [location.pathname])

  const hassEnabled = plugins.some((p) => p.meta.id === 'home-assistant' && p.enabled)
  const plcEnabled = plugins.some((p) => p.meta.id === 'beckhoff-plc' && p.enabled)

  return (
    // The nav lives inside the bar and wraps onto its own full-width row when
    // opened on a phone, so there is only ever one copy of it in the DOM.
    <header className="topbar">
      <a className="brand" href="connections">
        mqtt<span>view</span>
      </a>
      <button
        className="hamburger"
        aria-label="Toggle navigation"
        aria-expanded={open}
        onClick={() => setOpen((v) => !v)}
      >
        ☰
      </button>
      <nav className={`nav ${open ? 'open' : ''}`}>
        <NavLink to="/connections">Connections</NavLink>
        {hassEnabled && <NavLink to="/home-assistant">Devices</NavLink>}
        {plcEnabled && <NavLink to="/beckhoff-plc">PLC</NavLink>}
        <NavLink to="/plugins">Plugins</NavLink>
        {can('admin') && <NavLink to="/users">Users</NavLink>}
        <NavLink to="/account">{user?.name || user?.email}</NavLink>
        {/* Signing out of mqttview would do nothing in Home Assistant mode:
            the next request arrives authenticated again. Offering the link
            would be a button that visibly fails. */}
        {mode !== 'ingress' && (
          <a
            href="#logout"
            onClick={(e) => {
              e.preventDefault()
              void signOut()
            }}
          >
            Sign out
          </a>
        )}
      </nav>
    </header>
  )
}
