import { act, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it, vi } from 'vitest'
import { App } from '../App'
import { Auth } from '../pages/Auth'
import { Files } from '../pages/Files'
import { Jobs } from '../pages/Jobs'
import { Mods } from '../pages/Mods'
import { NewInstance } from '../pages/NewInstance'
import { Settings } from '../pages/Settings'
import { Console } from '../pages/Console'
import { UploadPanel } from '../uploads'
import { instance, job, json, mockAPI, owner, renderUI, session, settings, viewer } from './helpers'

describe('authentication and bootstrap', () => {
  it('shows a real connection error with a retry control', async () => {
    mockAPI(() => json({ error: 'Database is unavailable' }, 503)); renderUI(<App />)
    expect(await screen.findByText('Database is unavailable')).toBeVisible()
    expect(screen.getByRole('button', { name: 'Try again' })).toBeVisible()
  })
  it('opens the setup flow only when the server says setup is required', async () => {
    mockAPI(path => path === '/setup' ? { required: true } : json({ error: 'unexpected' }, 404)); renderUI(<App />)
    expect(await screen.findByRole('heading', { name: 'Set up your control panel' })).toBeVisible()
  })
  it('rejects mismatched setup passwords without sending credentials', async () => {
    const api = mockAPI(() => ({})); const user = userEvent.setup(); renderUI(<Auth setup />)
    await user.type(screen.getByLabelText(/Setup token/), 'token'); await user.type(screen.getByLabelText('Username'), 'owner')
    await user.type(screen.getByLabelText(/^Password/), 'a-strong-password'); await user.type(screen.getByLabelText('Confirm password'), 'a-different-password')
    await user.click(screen.getByRole('button', { name: 'Create owner account' }))
    expect(await screen.findByText('The passwords do not match.')).toBeVisible(); expect(api).not.toHaveBeenCalled()
  })
  it('sends an optional TOTP code and bootstraps the returned cookie session', async () => {
    const calls: { path: string; body: unknown }[] = []
    mockAPI((path, init) => { calls.push({ path, body: init.body ? JSON.parse(String(init.body)) : undefined }); return session })
    const user = userEvent.setup(); renderUI(<Auth setup={false} />)
    await user.type(screen.getByLabelText('Username'), 'owner'); await user.type(screen.getByLabelText('Password'), 'a-strong-password')
    await user.type(screen.getByLabelText(/Two-factor code/), '123456'); await user.click(screen.getByRole('button', { name: 'Sign in' }))
    await waitFor(() => expect(calls.some(call => call.path === '/auth/session')).toBe(true))
    expect(calls[0]).toEqual({ path: '/auth/login', body: { username: 'owner', password: 'a-strong-password', code: '123456' } })
  })
  it('renders the complete navigation without inventing instances for a viewer', async () => {
    mockAPI(path => path === '/setup' ? { required: false } : path === '/auth/session' ? { ...session, user: viewer } : [])
    renderUI(<App />)
    expect(await screen.findByRole('heading', { name: 'No instances available' })).toBeVisible()
    expect(screen.getByText('Ask the owner to grant you access to an instance.')).toBeVisible()
    expect(screen.queryByRole('link', { name: 'Add an instance' })).not.toBeInTheDocument()
    expect(screen.getByRole('link', { name: 'Application updates' })).toBeVisible()
  })
})

describe('instance creation', () => {
  it('loads authoritative choices, confirms, and reports an accepted job rather than a created server', async () => {
    let created: Record<string, unknown> | undefined
    mockAPI((path, init) => {
      if (path.startsWith('/fabric/versions')) return { game_versions: ['test-game'], loader_versions: ['test-loader'], installer_versions: ['test-installer'] }
      if (path === '/java') return [{ path: '/usr/bin/java', version: 'test-java', major: 99 }]
      if (path === '/instances') { created = JSON.parse(String(init.body)); return job }
      return []
    })
    const user = userEvent.setup(); renderUI(<NewInstance user={owner} />)
    await screen.findByRole('option', { name: /test-java/ })
    await user.type(screen.getByLabelText('Instance name'), 'Test survival')
    await user.selectOptions(screen.getByLabelText('Minecraft version'), 'test-game')
    await user.selectOptions(screen.getByLabelText('Fabric loader'), 'test-loader')
    expect(screen.getByLabelText('Fabric installer')).toHaveValue('test-installer')
    await user.selectOptions(screen.getByLabelText(/Java runtime/), '/usr/bin/java')
    await user.click(screen.getByRole('checkbox', { name: /Minecraft EULA/ }))
    await user.click(screen.getByRole('button', { name: 'Create Fabric server' }))
    expect(created).toBeUndefined()
    await user.click(screen.getByRole('button', { name: 'Create instance' }))
    await waitFor(() => expect(created).toMatchObject({ mode: 'create', name: 'Test survival', eula: true, minecraft_version: 'test-game', installer_version: 'test-installer' }))
    expect(await screen.findByText('Job queued')).toBeVisible()
    expect(screen.queryByText('Instance created.')).not.toBeInTheDocument()
  })
})

describe('file safety', () => {
  it('keeps viewer controls read-only and never exposes mutation/import controls', async () => {
    mockAPI(() => [{ name: 'readme.txt', path: 'readme.txt', size: 10, is_dir: false, modified_at: '2026-11-02T00:00:00Z' }])
    renderUI(<Files instance={instance} user={viewer} />)
    expect(await screen.findByRole('button', { name: 'readme.txt' })).toBeVisible()
    expect(screen.queryByText('Create a folder')).not.toBeInTheDocument()
    expect(screen.queryByRole('button', { name: 'Recovery' })).not.toBeInTheDocument()
    expect(screen.queryByRole('heading', { name: 'Import an archive or folder' })).not.toBeInTheDocument()
  })
  it('queues text changes with the original etag after explicit confirmation', async () => {
    let body: Record<string, unknown> | undefined
    mockAPI((path, init) => {
      if (init.method === 'POST') { body = JSON.parse(String(init.body)); return job }
      if (path.includes('preview=true')) return { path: 'config.txt', content: 'original', etag: 'revision-1' }
      return [{ name: 'config.txt', path: 'config.txt', size: 8, is_dir: false }]
    })
    const user = userEvent.setup(); renderUI(<Files instance={instance} user={owner} />)
    await user.click(await screen.findByRole('button', { name: 'config.txt' }))
    const editor = await screen.findByLabelText('File content'); await user.clear(editor); await user.type(editor, 'changed')
    await user.click(screen.getByRole('button', { name: 'Queue text edit' })); expect(body).toBeUndefined()
    await user.click(screen.getByRole('button', { name: 'Apply change' }))
    await waitFor(() => expect(body).toEqual({ operation: 'write', path: 'config.txt', content: 'changed', etag: 'revision-1', confirm: true }))
    expect(screen.getByLabelText('File content')).toHaveValue('changed')
  })
  it('surfaces stale-edit failures without replacing the local draft', async () => {
    mockAPI((path, init) => init.method === 'POST' ? json({ error: 'This file changed. Reload it first.' }, 409) : path.includes('preview=true') ? { path: 'config.txt', content: 'old', etag: 'old' } : [{ name: 'config.txt', path: 'config.txt', size: 3, is_dir: false }])
    const user = userEvent.setup(); renderUI(<Files instance={instance} user={owner} />)
    await user.click(await screen.findByRole('button', { name: 'config.txt' })); const editor = await screen.findByLabelText('File content')
    await user.type(editor, '-draft'); await user.click(screen.getByRole('button', { name: 'Queue text edit' })); await user.click(screen.getByRole('button', { name: 'Apply change' }))
    expect(await screen.findByText('This file changed. Reload it first.')).toBeVisible(); expect(editor).toHaveValue('old-draft')
  })
  it('lists retained paths only when recovery is requested', async () => {
    const api = mockAPI(path => path.endsWith('/recovery') ? [{ token: 'recovery-token', path: 'old-world', created_at: 1_793_577_600 }] : [])
    const user = userEvent.setup(); renderUI(<Files instance={instance} user={owner} />)
    expect(api.mock.calls.some(call => String(call[0]).endsWith('/recovery'))).toBe(false)
    await user.click(screen.getByRole('button', { name: 'Recovery' })); expect(await screen.findByText('old-world')).toBeVisible()
  })
  it('does not let a late file preview replace a newer selected file', async () => {
    let resolveFirst: () => void = () => {}
    const first = new Promise<void>(resolve => { resolveFirst = resolve })
    mockAPI(async path => {
      if (path.includes('path=a.txt') && path.includes('preview=true')) { await first; return { path: 'a.txt', content: 'first file', etag: 'a' } }
      if (path.includes('preview=true')) return { path: 'b.txt', content: 'second file', etag: 'b' }
      return ['a.txt', 'b.txt'].map(name => ({ name, path: name, size: 10, is_dir: false }))
    })
    const user = userEvent.setup(); renderUI(<Files instance={instance} user={owner} />)
    await user.click(await screen.findByRole('button', { name: 'a.txt' })); await user.click(screen.getByRole('button', { name: 'b.txt' }))
    expect(await screen.findByLabelText('File content')).toHaveValue('second file')
    await act(async () => { resolveFirst(); await first })
    expect(screen.getByLabelText('File content')).toHaveValue('second file')
  })
  it('invalidates a previously reviewed upload when another source is selected', async () => {
    const reset = vi.fn(); const user = userEvent.setup()
    renderUI(<UploadPanel scope="test" onComplete={() => {}} onReset={reset} />)
    await user.upload(screen.getByLabelText('Choose a ZIP or tar.gz archive'), new File(['archive'], 'new.zip', { type: 'application/zip' }))
    expect(reset).toHaveBeenCalledOnce()
    await user.click(screen.getByRole('radio', { name: 'Folder' })); expect(reset).toHaveBeenCalledTimes(2)
  })
})

describe('jobs, mods, console, and secret configuration', () => {
  it('renders server job diagnostics and hides cancellation beyond a commit barrier', async () => {
    mockAPI(() => ({ job: { ...job, state: 'failed', error: 'Hash mismatch; staged files retained.', cancellable: false }, steps: [{ time: job.created_at, progress: 10, message: 'Checking hashes' }] }))
    renderUI(<Jobs id={job.id} user={owner} instances={[instance]} />)
    expect(await screen.findByText('Hash mismatch; staged files retained.')).toBeVisible()
    expect(screen.getByText('Checking hashes')).toBeVisible(); expect(screen.queryByRole('button', { name: 'Cancel job' })).not.toBeInTheDocument()
  })
  it('applies a mod plan with its hash and never offers management for external content', async () => {
    let apply: Record<string, unknown> | undefined
    mockAPI((path, init) => {
      if (path.endsWith('/mods/plan')) return { plan_hash: 'verified-plan', actions: [{ project_id: 'project-a', name: 'Managed mod', from_version: '1', to_version: '2', filename: 'mod.jar', reason: 'Compatible update' }], blocked: [], warnings: [] }
      if (path.endsWith('/mods/apply')) { apply = JSON.parse(String(init.body)); return job }
      if (path.endsWith('/mods/policy')) return { mode: 'manual', interval_hours: 24, window_start: '04:00', window_end: '05:00', timezone: 'UTC', postpone_players: true }
      return [{ filename: 'unknown.jar', sha512: 'hash', name: 'External jar', managed: false, pinned: false, update_available: false }]
    })
    const user = userEvent.setup(); renderUI(<Mods instance={instance} user={owner} />)
    expect(await screen.findByText('Preserved as external content')).toBeVisible(); expect(screen.queryByRole('button', { name: 'Remove' })).not.toBeInTheDocument()
    await user.click(screen.getByRole('button', { name: 'Plan managed updates' })); await user.click(await screen.findByRole('button', { name: 'Apply reviewed plan' }))
    await user.type(screen.getByLabelText(/Type “Test survival”/), instance.name); await user.click(screen.getByRole('button', { name: 'Apply mod plan' }))
    await waitFor(() => expect(apply).toEqual({ projects: [], plan_hash: 'verified-plan', confirm: true }))
  })
  it('follows a serialized console command job before showing its output', async () => {
    mockAPI((path, init) => path.endsWith('/status') ? { state: 'ready', ready: true, players: [], uptime_seconds: 1, memory_bytes: 1, cpu_seconds: 1 } : path.endsWith('/logs') ? [] : path.endsWith('/console') ? job : path.startsWith('/jobs/') ? { job: { ...job, state: 'succeeded', result: { output: 'There are 0 players online.' } }, steps: [] } : [])
    const user = userEvent.setup(); renderUI(<Console instance={instance} user={owner} />)
    await waitFor(() => expect(screen.getByRole('button', { name: 'Send command' })).toBeDisabled())
    await user.type(screen.getByLabelText('Minecraft command'), 'list'); await waitFor(() => expect(screen.getByRole('button', { name: 'Send command' })).toBeEnabled())
    await user.click(screen.getByRole('button', { name: 'Send command' })); expect(await screen.findByText('There are 0 players online.')).toBeVisible()
  })
  it('preserves encrypted storage credentials unless explicit replacement is selected', async () => {
    let saved: Record<string, unknown> | undefined
    mockAPI((path, init) => { if (init.method === 'PUT') saved = JSON.parse(String(init.body)); return settings })
    const user = userEvent.setup(); renderUI(<Settings user={owner} instances={[instance]} />)
    await user.click(screen.getByRole('button', { name: 'Panel' })); await screen.findByRole('heading', { name: 'R2 / S3 backup storage' })
    expect(screen.queryByLabelText('Secret key')).not.toBeInTheDocument()
    await user.click(screen.getByLabelText('Enable remote backup storage'))
    await user.click(screen.getByRole('button', { name: 'Save storage configuration' })); await user.click(screen.getByRole('button', { name: 'Save storage' }))
    await waitFor(() => expect(saved).toEqual({ storage: { enabled: false } }))
  })
})
