import { useEffect, useRef, useState } from 'react'
import { useQueryClient } from '@tanstack/react-query'
import { Activity, Archive, CalendarClock, Download, FileText, Globe2, LayoutDashboard, LogOut, Menu, Package, Plus, Server, Settings2, ShieldCheck, Terminal, Users, X } from 'lucide-react'
import { API, APIError, canAdmin, request, setSession, useAPI, type Instance, type Session } from './api'
import { Button, ConfirmProvider, Empty, ErrorNotice, Loading, Notice, PageHeading, Section } from './components'
import { Auth } from './pages/Auth'
import { Automation } from './pages/Automation'
import { Backups } from './pages/Backups'
import { Console } from './pages/Console'
import { Dashboard } from './pages/Dashboard'
import { Files } from './pages/Files'
import { Jobs } from './pages/Jobs'
import { Mods } from './pages/Mods'
import { NewInstance } from './pages/NewInstance'
import { Players } from './pages/Players'
import { Settings } from './pages/Settings'
import { Updates } from './pages/Updates'
import { Worlds } from './pages/Worlds'

const sections = [
  { id: 'dashboard', label: 'Dashboard', icon: LayoutDashboard },
  { id: 'console', label: 'Console', icon: Terminal },
  { id: 'files', label: 'Files', icon: FileText },
  { id: 'worlds', label: 'Worlds', icon: Globe2 },
  { id: 'mods', label: 'Mods', icon: Package },
  { id: 'backups', label: 'Backups', icon: Archive },
  { id: 'players', label: 'Players', icon: Users },
  { id: 'automation', label: 'Automation', icon: CalendarClock },
]
export function parseRoute(hash: string) {
  const parts = hash.replace(/^#\/?/, '').split('/').filter(Boolean)
  if (!parts.length) return { page: 'home' }
  if (parts[0] === 'instances' && /^[a-f0-9]{32}$/.test(parts[1] ?? '')) return { page: parts[2] ?? 'dashboard', instanceID: parts[1] }
  if (parts[0] === 'jobs') return { page: 'jobs', jobID: parts[1] }
  if (['new', 'settings', 'updates'].includes(parts[0])) return { page: parts[0] }
  return { page: 'not-found' }
}

export function App() {
  const client = useQueryClient()
  const setup = useAPI<{ required: boolean }>('/setup')
  const session = useAPI<Session | null>('/auth/session', { enabled: setup.data?.required === false })
  const instances = useAPI<Instance[]>('/instances', { enabled: !!session.data, interval: 15_000 })
  const [hash, setHash] = useState(window.location.hash)
  const [menuOpen, setMenuOpen] = useState(false)
  const [live, setLive] = useState(false)
  const [logoutBusy, setLogoutBusy] = useState(false)
  const [logoutError, setLogoutError] = useState<unknown>(null)
  const [lastInstanceID, setLastInstanceID] = useState<string>()
  const sidebar = useRef<HTMLElement>(null)
  const route = parseRoute(hash)
  const user = session.data?.user
  useEffect(() => { if (route.instanceID) setLastInstanceID(route.instanceID) }, [route.instanceID])
  useEffect(() => {
    if (!menuOpen) return
    const previousOverflow = document.body.style.overflow
    document.body.style.overflow = 'hidden'
    sidebar.current?.querySelector<HTMLElement>('button.close-menu')?.focus()
    function keyboard(event: KeyboardEvent) {
      if (event.key === 'Escape') { event.preventDefault(); setMenuOpen(false) }
      if (event.key !== 'Tab') return
      const focusable = sidebar.current?.querySelectorAll<HTMLElement>('a[href], button:not([disabled])')
      if (!focusable?.length) return
      const first = focusable[0], last = focusable[focusable.length - 1]
      if (event.shiftKey && document.activeElement === first) { event.preventDefault(); last.focus() }
      else if (!event.shiftKey && document.activeElement === last) { event.preventDefault(); first.focus() }
    }
    document.addEventListener('keydown', keyboard)
    return () => { document.body.style.overflow = previousOverflow; document.removeEventListener('keydown', keyboard); document.getElementById('navigation-toggle')?.focus() }
  }, [menuOpen])
  useEffect(() => {
    if (typeof window.matchMedia !== 'function') return
    const mobile = window.matchMedia('(max-width: 900px)')
    const changed = () => { if (!mobile.matches) setMenuOpen(false) }
    mobile.addEventListener('change', changed)
    return () => mobile.removeEventListener('change', changed)
  }, [])
  useEffect(() => {
    const listener = () => { setHash(window.location.hash); setMenuOpen(false); window.scrollTo({ top: 0 }) }
    window.addEventListener('hashchange', listener)
    return () => window.removeEventListener('hashchange', listener)
  }, [])
  useEffect(() => { setSession(session.data ?? undefined) }, [session.data])
  useEffect(() => {
    const expired = () => { setSession(); client.setQueryData(['/auth/session'], null); client.removeQueries({ predicate: query => !['/setup', '/auth/session'].includes(String(query.queryKey[0])) }) }
    window.addEventListener('session-expired', expired)
    return () => window.removeEventListener('session-expired', expired)
  }, [client])
  useEffect(() => {
    if (!user || typeof EventSource === 'undefined') return
    const source = new EventSource(`${API}/events`, { withCredentials: true })
    let timer: ReturnType<typeof setTimeout> | undefined
    const invalidate = () => { if (!timer) timer = setTimeout(() => { timer = undefined; void client.invalidateQueries({ predicate: query => !['/setup', '/auth/session'].includes(String(query.queryKey[0])) }) }, 300) }
    source.onopen = () => setLive(true)
    source.onerror = () => setLive(false)
    source.onmessage = invalidate
    for (const name of ['job', 'instance', 'invalidate']) source.addEventListener(name, invalidate)
    return () => { source.close(); clearTimeout(timer); setLive(false) }
  }, [client, user?.id])
  useEffect(() => {
    if (route.page === 'home' && instances.data?.length) window.location.hash = `#/instances/${instances.data[0].id}/dashboard`
  }, [route.page, instances.data])
  useEffect(() => { document.title = `${sections.find(section => section.id === route.page)?.label ?? ({ jobs: 'Jobs', settings: 'Settings', updates: 'Application updates', new: 'Add instance' }[route.page] || 'Administration')} · New Empire` }, [route.page])
  async function logout() {
    setLogoutBusy(true); setLogoutError(null)
    try { await request('/auth/logout', { method: 'POST' }); setSession(); client.clear(); window.location.hash = ''; window.location.reload() } catch (cause) { setLogoutError(cause) } finally { setLogoutBusy(false) }
  }
  if (setup.isPending) return <main className="boot-screen"><Loading label="Connecting to your control panel…" /></main>
  if (setup.isError) return <main className="boot-screen"><h1>Panel connection unavailable</h1><ErrorNotice error={setup.error} retry={() => { void setup.refetch() }} /></main>
  if (setup.data.required) return <Auth setup />
  if (session.isPending) return <main className="boot-screen"><Loading label="Checking your session…" /></main>
  if (session.isError && (!(session.error instanceof APIError) || session.error.status !== 401)) return <main className="boot-screen"><h1>Session could not be checked</h1><ErrorNotice error={session.error} retry={() => { void session.refetch() }} /></main>
  if (!session.data || !user) return <Auth setup={false} />
  const instance = instances.data?.find(item => item.id === (route.instanceID ?? lastInstanceID)) ?? (!route.instanceID ? instances.data?.[0] : undefined)
  function page() {
    if (route.page === 'jobs') return <Jobs user={user!} instances={instances.data ?? []} id={route.jobID} />
    if (route.page === 'updates') return <Updates user={user!} />
    if (route.page === 'settings') return <Settings key={instance?.id ?? 'panel'} user={user!} instance={instance} instances={instances.data ?? []} />
    if (route.page === 'new' || (route.page === 'home' && !instances.data?.length)) return canAdmin(user!) ? <NewInstance user={user!} /> : <><PageHeading title="No instances available" /><Empty title="No instance access is assigned">Ask the owner to grant you access to an instance.</Empty></>
    if (route.instanceID && !instance) return <><PageHeading title="Instance unavailable" /><Empty title="This instance was not returned by the server">It may have been removed, or your account may not have access. Select another instance.</Empty></>
    if (!instance) return <><PageHeading title="Page not found" /><Empty title="This page is not available" action={<a className="button button-primary" href="#/">Go to instances</a>}>Choose a page from the navigation.</Empty></>
    const props = { instance, user: user! }
    switch (route.page) {
      case 'dashboard': return <Dashboard {...props} />
      case 'console': return <Console {...props} />
      case 'files': return <Files {...props} />
      case 'worlds': return <Worlds {...props} />
      case 'mods': return <Mods {...props} />
      case 'backups': return <Backups {...props} />
      case 'players': return <Players {...props} />
      case 'automation': return <Automation {...props} />
      default: return <Empty title="Page not found">Choose another page from the navigation.</Empty>
    }
  }
  return <ConfirmProvider><a className="skip-link" tabIndex={menuOpen ? -1 : undefined} href="#main-content" onClick={event => { event.preventDefault(); document.getElementById('main-content')?.focus() }}>Skip to content</a><div className="app-shell">
    {menuOpen && <button tabIndex={-1} className="nav-backdrop" aria-label="Close navigation" onClick={() => setMenuOpen(false)} />}
    <aside ref={sidebar} className={`sidebar ${menuOpen ? 'open' : ''}`} role={menuOpen ? 'dialog' : undefined} aria-modal={menuOpen ? true : undefined} aria-label="Main navigation"><a className="brand" href="#/"><span className="brand-mark"><ShieldCheck size={25} aria-hidden="true" /></span><span>New Empire<small>Server Control</small></span></a><Button variant="ghost" className="close-menu" onClick={() => setMenuOpen(false)} aria-label="Close navigation"><X size={21} /></Button>
      <div className="nav-group-label">Instance workspace</div><nav aria-label="Instance pages">{sections.map(section => instance ? <a key={section.id} href={`#/instances/${instance.id}/${section.id}`} className={route.page === section.id ? 'active' : ''} aria-current={route.page === section.id ? 'page' : undefined}><section.icon size={18} aria-hidden="true" />{section.label}</a> : <span className="nav-disabled" key={section.id}><section.icon size={18} aria-hidden="true" />{section.label}</span>)}</nav>
      <div className="nav-divider" /><nav aria-label="Panel pages"><a href="#/jobs" className={route.page === 'jobs' ? 'active' : ''} aria-current={route.page === 'jobs' ? 'page' : undefined}><Activity size={18} />Jobs</a><a href={instance ? `#/instances/${instance.id}/settings` : '#/settings'} className={route.page === 'settings' ? 'active' : ''} aria-current={route.page === 'settings' ? 'page' : undefined}><Settings2 size={18} />Settings</a><a href="#/updates" className={route.page === 'updates' ? 'active' : ''} aria-current={route.page === 'updates' ? 'page' : undefined}><Download size={18} />Application updates</a></nav>
      <div className="sidebar-foot"><span className={`connection ${live ? 'connected' : ''}`}><span aria-hidden="true" />{live ? 'Live updates connected' : 'Polling available'}</span><small>Isolated instances. Deliberate changes.</small></div>
    </aside>
    <div className="workspace" inert={menuOpen}><header className="topbar"><Button id="navigation-toggle" variant="ghost" className="open-menu" aria-label="Open navigation" aria-expanded={menuOpen} onClick={() => setMenuOpen(true)}><Menu size={22} /></Button><Server size={18} className="instance-icon" aria-hidden="true" /><label className="instance-selector"><span className="sr-only">Selected instance</span><select value={instance?.id ?? ''} onChange={event => { if (event.target.value) window.location.hash = `#/instances/${event.target.value}/${sections.some(section => section.id === route.page) || route.page === 'settings' ? route.page : 'dashboard'}` }}><option value="" disabled>{instances.data?.length ? 'Select an instance' : 'No instances yet'}</option>{instances.data?.map(item => <option value={item.id} key={item.id}>{item.name}</option>)}</select></label>{canAdmin(user) && <a className="button button-ghost add-instance" href="#/new" aria-label="Add an instance"><Plus size={17} /><span>Add instance</span></a>}<div className="account-menu"><div><strong>{user.username}</strong><small>{user.role}</small></div><Button variant="ghost" aria-label="Sign out" busy={logoutBusy} onClick={() => { void logout() }}><LogOut size={18} /></Button></div></header>
      <main id="main-content" tabIndex={-1} className="main-content">{!!logoutError && <ErrorNotice error={logoutError} />}{instances.isPending ? <Loading label="Loading your instances…" /> : instances.isError ? <ErrorNotice error={instances.error} retry={() => { void instances.refetch() }} /> : <div key={`${route.instanceID ?? ''}-${route.page}-${route.jobID ?? ''}`}>{page()}</div>}</main>
      <footer className="workspace-footer"><span>New Empire Server Control</span><span>Source-available · PolyForm Shield 1.0.0</span></footer>
    </div>
  </div></ConfirmProvider>
}
