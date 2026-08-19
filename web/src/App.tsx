import { useEffect, useState } from 'react'
import { BrowserRouter, Navigate, NavLink, Route, Routes, useLocation } from 'react-router-dom'
import { api } from './api/client'
import type { PluginInfo } from './api/types'
import { AuthProvider, useAuth } from './auth/AuthContext'
import { Spinner } from './components/common'
import { Account } from './pages/Account'
import { ConnectionForm } from './pages/ConnectionForm'
import { Connections } from './pages/Connections'
import { Explorer } from './pages/Explorer'
import { HomeAssistant } from './pages/HomeAssistant'
import { Login } from './pages/Login'
import { Plugins } from './pages/Plugins'
import { Users } from './pages/Users'

export default function App() {
  return (
    <BrowserRouter>
      <AuthProvider>
        <Shell />
      </AuthProvider>
    </BrowserRouter>
  )
}

function Shell() {
  const { user, loading } = useAuth()

  if (loading) return <Spinner label="Starting mqttview…" />
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
          <Route path="/users" element={<Users />} />
          <Route path="/account" element={<Account />} />
          <Route path="*" element={<Navigate to="/connections" replace />} />
        </Routes>
      </main>
    </div>
  )
}

function Navigation() {
  const { user, signOut, can } = useAuth()
  const [open, setOpen] = useState(false)
  const [plugins, setPlugins] = useState<PluginInfo[]>([])
  const location = useLocation()

  useEffect(() => {
    api.plugins().then(setPlugins).catch(() => undefined)
  }, [])

  // Close the mobile menu whenever the route changes.
  useEffect(() => setOpen(false), [location.pathname])

  const hassEnabled = plugins.some((p) => p.meta.id === 'home-assistant' && p.enabled)

  return (
    // The nav lives inside the bar and wraps onto its own full-width row when
    // opened on a phone, so there is only ever one copy of it in the DOM.
    <header className="topbar">
      <a className="brand" href="/connections">
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
        <NavLink to="/plugins">Plugins</NavLink>
        {can('admin') && <NavLink to="/users">Users</NavLink>}
        <NavLink to="/account">{user?.name || user?.email}</NavLink>
        <a
          href="#logout"
          onClick={(e) => {
            e.preventDefault()
            void signOut()
          }}
        >
          Sign out
        </a>
      </nav>
    </header>
  )
}
