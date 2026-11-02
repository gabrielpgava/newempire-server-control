import { act, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it } from 'vitest'
import { formatDate, type Backup, type Mod, type UpdateState, type User } from '../api'
import { Backups } from '../pages/Backups'
import { Dashboard } from '../pages/Dashboard'
import { Mods } from '../pages/Mods'
import { Settings } from '../pages/Settings'
import { Updates } from '../pages/Updates'
import { instance, job, json, mockAPI, owner, renderUI, settings, viewer } from './helpers'

const admin: User = { ...owner, role: 'admin' }
const operator: User = { ...viewer, role: 'operator', instance_ids: [instance.id] }
const updateState: UpdateState = { current_version: 'test-current', repository: 'official/repository', available: { version: 'test-next', notes: '', size: 100, prerelease: false }, state: 'staged', history: [{ version: 'test-previous' }], policy: { mode: 'notify', channel: 'stable', interval_hours: 24, window_start: '04:00', window_end: '05:00', timezone: 'UTC' } }
const modPolicy = { mode: 'manual', interval_hours: 24, window_start: '04:00', window_end: '05:00', timezone: 'UTC', postpone_players: true }
const backup: Backup = { id: 'backup-id', instance_id: instance.id, created_at: job.created_at, size: 100, sha256: 'expected-sha256', note: 'Before changes', verified: true, local: true }
const identified: Mod = { filename: 'identified.jar', sha512: 'exact-file-hash', name: 'Identified external mod', managed: false, pinned: false, update_available: false, project_id: 'identified-project', version_id: 'identified-version' }

function updateAPI() {
  const writes: { path: string; method: string; body: unknown }[] = []
  mockAPI((path, init) => {
    if (init.method) writes.push({ path, method: init.method, body: init.body ? JSON.parse(String(init.body)) : undefined })
    return init.method === 'PUT' ? updateState.policy : init.method === 'POST' ? job : updateState
  })
  return writes
}

describe('panel update request and permission contracts', () => {
  it.each([
    { kind: 'check', button: 'Check official releases', confirmation: undefined, body: {} },
    { kind: 'stage', button: 'Stage and verify release', confirmation: undefined, body: {} },
    { kind: 'apply', button: 'Apply verified staged update', confirmation: 'Apply verified panel update', body: { confirm: true } },
    { kind: 'rollback', button: 'Roll back panel', confirmation: 'Roll back panel', body: { confirm: true } },
  ])('sends explicit JSON for $kind, with confirmation only for applying changes', async ({ kind, button, confirmation, body }) => {
    const writes = updateAPI(); const user = userEvent.setup(); renderUI(<Updates user={owner} />)
    await user.click(await screen.findByRole('button', { name: button }))
    if (confirmation) {
      expect(writes).toHaveLength(0)
      await user.click(within(screen.getByRole('dialog')).getByRole('button', { name: confirmation }))
    }
    await waitFor(() => expect(writes).toEqual([{ path: `/updates/${kind}`, method: 'POST', body }]))
  })
  it.each(['Apply verified staged update', 'Roll back panel'])('does not mutate after cancelling %s', async button => {
    const writes = updateAPI(); const user = userEvent.setup(); renderUI(<Updates user={owner} />)
    await user.click(await screen.findByRole('button', { name: button }))
    await user.click(within(screen.getByRole('dialog')).getByText('Cancel', { selector: 'button' }))
    expect(writes).toHaveLength(0)
  })
  it('lets administrators read but not mutate release or policy state', async () => {
    const writes = updateAPI(); renderUI(<Updates user={admin} />)
    expect(await screen.findByText('test-current')).toBeVisible()
    expect(screen.getByText(/Only the owner can check, stage, apply/)).toBeVisible()
    expect(screen.getByLabelText('Check interval (hours)')).toBeDisabled()
    for (const name of ['Check official releases', 'Stage and verify release', 'Apply verified staged update', 'Roll back panel', 'Save panel update policy', 'Postpone automatic updates 24h']) expect(screen.queryByRole('button', { name })).not.toBeInTheDocument()
    expect(writes).toHaveLength(0)
  })
  it.each([operator, viewer])('does not request admin-only update information for $role', user => {
    const api = mockAPI(() => updateState); renderUI(<Updates user={user} />)
    expect(screen.getByText(/administrator or owner role is required to view/)).toBeVisible()
    expect(api).not.toHaveBeenCalled()
  })
  it('enforces the 168-hour policy limit and sends the supported policy fields', async () => {
    const writes = updateAPI(); const user = userEvent.setup(); renderUI(<Updates user={owner} />)
    const interval = await screen.findByLabelText('Check interval (hours)')
    expect(interval).toHaveAttribute('max', '168')
    await user.clear(interval); await user.type(interval, '169')
    await user.click(screen.getByRole('button', { name: 'Save panel update policy' })); expect(writes).toHaveLength(0)
    await user.clear(interval); await user.type(interval, '168')
    await user.click(screen.getByRole('button', { name: 'Save panel update policy' }))
    await waitFor(() => expect(writes).toEqual([{ path: '/updates/policy', method: 'PUT', body: { ...updateState.policy, interval_hours: 168 } }]))
  })
  it('postpones by exactly 24 hours and shows the returned deadline without inventing one', async () => {
    let body: unknown
    const until = 1_793_664_000
    let savedPolicy = updateState.policy
    mockAPI((path, init) => { if (path === '/updates/postpone') { body = JSON.parse(String(init.body)); savedPolicy = { ...savedPolicy, postpone_until: until }; return savedPolicy }; return { ...updateState, policy: savedPolicy } })
    const user = userEvent.setup(); renderUI(<Updates user={owner} />)
    expect(screen.queryByText(/Automatic update postponement deadline/)).not.toBeInTheDocument()
    await user.click(await screen.findByRole('button', { name: 'Postpone automatic updates 24h' }))
    await waitFor(() => expect(body).toEqual({ hours: 24 }))
    expect(await screen.findByText(formatDate(until))).toBeVisible()
    expect(screen.getByText(/Automatic update postponement deadline/)).toBeVisible()
  })
  it('shows an existing postponement deadline from the saved policy to administrators', async () => {
    const until = 1_793_664_000
    mockAPI(() => ({ ...updateState, policy: { ...updateState.policy, postpone_until: until } }))
    renderUI(<Updates user={admin} />)
    expect(await screen.findByText(formatDate(until))).toBeVisible()
  })
  it('keeps postponement on ordinary policy saves and follows refreshed server deadlines', async () => {
    const until = 1_793_664_000, newer = until + 3600
    let savedPolicy = updateState.policy
    const writes: unknown[] = []
    mockAPI((path, init) => {
      if (path === '/updates/postpone') { savedPolicy = { ...savedPolicy, postpone_until: until }; return savedPolicy }
      if (path === '/updates/policy') { const body = JSON.parse(String(init.body)); writes.push(body); savedPolicy = { ...body, postpone_until: savedPolicy.postpone_until }; return savedPolicy }
      return { ...updateState, policy: savedPolicy }
    })
    const user = userEvent.setup(); const { client } = renderUI(<Updates user={owner} />)
    await user.click(await screen.findByRole('button', { name: 'Postpone automatic updates 24h' }))
    expect(await screen.findByText(formatDate(until))).toBeVisible()
    const interval = screen.getByLabelText('Check interval (hours)')
    await user.clear(interval); await user.type(interval, '12')
    await user.click(screen.getByRole('button', { name: 'Save panel update policy' }))
    await waitFor(() => expect(writes).toEqual([{ ...updateState.policy, interval_hours: 12 }]))
    expect(screen.getByText(formatDate(until))).toBeVisible()
    savedPolicy = { ...savedPolicy, postpone_until: newer }
    await act(async () => { await client.invalidateQueries({ queryKey: ['/updates'] }) })
    expect(await screen.findByText(formatDate(newer))).toBeVisible()
    expect(screen.queryByText(formatDate(until))).not.toBeInTheDocument()
    savedPolicy = { ...savedPolicy, postpone_until: 0 }
    await act(async () => { await client.invalidateQueries({ queryKey: ['/updates'] }) })
    await waitFor(() => expect(screen.queryByText(/Automatic update postponement deadline/)).not.toBeInTheDocument())
  })
})

describe('version-scoped mod requests and explicit adoption', () => {
  it('includes the exact URL-encoded instance version in Modrinth search', async () => {
    let search: string | undefined
    mockAPI(path => { if (path.startsWith('/mods/search?')) search = path; return path.endsWith('/mods/policy') ? modPolicy : [] })
    const user = userEvent.setup(); const scoped = { ...instance, minecraft_version: 'test game+build' }
    renderUI(<Mods instance={scoped} user={admin} />)
    await user.type(screen.getByLabelText('Search Fabric mods'), 'mod & content'); await user.click(screen.getByRole('button', { name: 'Search' }))
    await waitFor(() => expect(search).toBe('/mods/search?q=mod%20%26%20content&minecraft_version=test%20game%2Bbuild'))
  })
  it('limits mod checks to 168 hours and preserves administrator policy access', async () => {
    let body: unknown
    mockAPI((path, init) => { if (init.method === 'PUT') body = JSON.parse(String(init.body)); return path.endsWith('/mods/policy') ? modPolicy : [] })
    const user = userEvent.setup(); renderUI(<Mods instance={instance} user={admin} />)
    const interval = await screen.findByLabelText('Check interval (hours)'); expect(interval).toHaveAttribute('max', '168')
    await user.clear(interval); await user.type(interval, '169'); await user.click(screen.getByRole('button', { name: 'Save update policy' })); expect(body).toBeUndefined()
    await user.clear(interval); await user.type(interval, '168'); await user.click(screen.getByRole('button', { name: 'Save update policy' }))
    await waitFor(() => expect(body).toEqual({ ...modPolicy, interval_hours: 168 }))
  })
  it('adopts only after explicit confirmation and does not request a JAR mutation', async () => {
    const writes: { path: string; body: unknown }[] = []
    mockAPI((path, init) => { if (init.method === 'POST') { writes.push({ path, body: JSON.parse(String(init.body)) }); return job }; return path.endsWith('/mods/policy') ? modPolicy : [identified] })
    const user = userEvent.setup(); renderUI(<Mods instance={instance} user={admin} />)
    await user.click(await screen.findByRole('button', { name: 'Adopt exact hash' })); expect(writes).toHaveLength(0)
    expect(screen.getByText(/must already be stopped and have a verified backup/)).toBeVisible()
    expect(screen.getByText(/does not change the JAR bytes/)).toBeVisible()
    await user.click(screen.getByRole('button', { name: 'Confirm adoption' }))
    await waitFor(() => expect(writes).toEqual([{ path: `/instances/${instance.id}/mods/manage`, body: { operation: 'adopt', project_id: identified.project_id, confirm: true } }]))
    expect(await screen.findByText('Job queued')).toBeVisible()
  })
  it('leaves content without both project and version identity preserved', async () => {
    mockAPI(path => path.endsWith('/mods/policy') ? modPolicy : [{ ...identified, project_id: undefined }, { ...identified, filename: 'no-version.jar', version_id: undefined }])
    renderUI(<Mods instance={instance} user={owner} />)
    await screen.findByText('no-version.jar')
    expect(screen.queryByRole('button', { name: 'Adopt exact hash' })).not.toBeInTheDocument()
  })
  it.each([operator, viewer])('keeps identified external content read-only for $role', async user => {
    mockAPI(path => path.endsWith('/mods/policy') ? modPolicy : [identified]); renderUI(<Mods instance={instance} user={user} />)
    await screen.findByText(identified.name!)
    expect(screen.queryByRole('button', { name: 'Adopt exact hash' })).not.toBeInTheDocument()
  })
  it('reports failed adoption preflight without claiming a successful change', async () => {
    mockAPI((path, init) => init.method === 'POST' ? json({ error: 'A stopped server and verified backup are required.' }, 409) : path.endsWith('/mods/policy') ? modPolicy : [identified])
    const user = userEvent.setup(); renderUI(<Mods instance={instance} user={owner} />)
    await user.click(await screen.findByRole('button', { name: 'Adopt exact hash' })); await user.click(screen.getByRole('button', { name: 'Confirm adoption' }))
    expect(await screen.findByText('A stopped server and verified backup are required.')).toBeVisible()
    expect(screen.queryByText('Job queued')).not.toBeInTheDocument()
  })
})

describe('immutable host configuration and property dirty state', () => {
  it.each([owner, admin])('keeps public URL read-only while allowing $role retention changes', async user => {
    let body: unknown
    mockAPI((path, init) => { if (init.method === 'PUT') body = JSON.parse(String(init.body)); return settings })
    const actor = userEvent.setup(); renderUI(<Settings user={user} instances={[instance]} />)
    await actor.click(screen.getByRole('button', { name: 'Panel' })); const url = await screen.findByLabelText('Public panel URL')
    expect(url).toHaveAttribute('readonly'); expect(url).toHaveValue(settings.public_url)
    expect(screen.getByText(/Managed by the root-owned host configuration/)).toBeVisible()
    const retained = screen.getByLabelText('Backups to retain'); await actor.clear(retained); await actor.type(retained, '5')
    await actor.click(screen.getByRole('button', { name: 'Save panel settings' }))
    await waitFor(() => expect(body).toEqual({ backup_retention: { enabled: true, keep_last: 5 } }))
  })
  it('does not submit the world-managed level-name property or assume an initial dirty flag', async () => {
    let body: unknown
    mockAPI((path, init) => { if (init.method === 'PUT') { body = JSON.parse(String(init.body)); return job }; return { properties: { motd: 'Original', pvp: 'true' }, restart_required: false } })
    const user = userEvent.setup(); renderUI(<Settings user={owner} instance={instance} instances={[instance]} />)
    const motd = await screen.findByLabelText('motd'); expect(screen.queryByLabelText('level-name')).not.toBeInTheDocument()
    expect(screen.queryByText('Applied property changes are pending a server restart.')).not.toBeInTheDocument()
    await user.clear(motd); await user.type(motd, 'Changed'); await user.click(screen.getByRole('button', { name: 'Apply properties' }))
    await user.click(within(screen.getByRole('dialog')).getByRole('button', { name: 'Apply properties' }))
    await waitFor(() => expect(body).toEqual({ properties: { motd: 'Changed', pvp: 'true' }, confirm: true }))
  })
  it('shows a pending-restart warning only for the reported dirty state', async () => {
    mockAPI(() => ({ properties: { motd: 'Applied change' }, restart_required: true }))
    renderUI(<Settings user={owner} instance={instance} instances={[instance]} />)
    expect(await screen.findByText('Applied property changes are pending a server restart.')).toBeVisible()
  })
})

describe('local and offsite backup boundaries', () => {
  it('fetches remote-only copies with JSON and waits for confirmed local availability before use', async () => {
    let local = false
    const writes: { path: string; body: unknown }[] = []
    const reads: string[] = []
    mockAPI((path, init) => {
      if (init.method === 'POST') { writes.push({ path, body: init.body ? JSON.parse(String(init.body)) : undefined }); return job }
      reads.push(path)
      if (path.endsWith('/preview')) return { files: [], file_count: 0, total_bytes: 0, warnings: [] }
      return [{ ...backup, local, offsite: { state: 'uploaded', object: { verification: 'head-metadata' } } }]
    })
    const user = userEvent.setup(); renderUI(<Backups instance={instance} user={owner} />)
    expect(await screen.findByText('not stored locally')).toBeVisible()
    expect(screen.getByText('uploaded')).toBeVisible(); expect(screen.getByText('Recorded verification: head-metadata')).toBeVisible()
    expect(screen.getByText(/not an independent check of remote bytes/)).toBeVisible()
    expect(screen.getByRole('button', { name: 'Verify' })).toBeDisabled()
    expect(screen.getByRole('button', { name: 'Restore preview' })).toBeDisabled()
    expect(screen.queryByRole('link', { name: /Download backup/ })).not.toBeInTheDocument()
    await user.click(screen.getByRole('button', { name: 'Fetch verified offsite copy' }))
    await waitFor(() => expect(writes).toEqual([{ path: `/instances/${instance.id}/backups/${backup.id}/fetch-offsite`, body: {} }]))
    expect(await screen.findByText('Job queued')).toBeVisible()
    expect(screen.getByRole('button', { name: 'Restore preview' })).toBeDisabled()
    expect(reads.some(path => path.endsWith('/preview'))).toBe(false)
    local = true
    await waitFor(() => expect(screen.getByRole('button', { name: 'Refresh backups' })).toBeEnabled())
    await user.click(screen.getByRole('button', { name: 'Refresh backups' }))
    await waitFor(() => expect(screen.getByRole('button', { name: 'Restore preview' })).toBeEnabled())
    expect(screen.getByRole('button', { name: 'Verify' })).toBeEnabled()
    expect(screen.getByRole('link', { name: /Download backup/ })).toHaveAttribute('href', `/api/v1/instances/${instance.id}/backups/${backup.id}/download`)
    await user.click(screen.getByRole('button', { name: 'Restore preview' }))
    await waitFor(() => expect(reads).toContain(`/instances/${instance.id}/backups/${backup.id}/preview`))
  })
  it.each([true, false])('keeps archived secrets and offsite fetch admin-only for operators (local=%s)', async local => {
    mockAPI(() => [{ ...backup, local, offsite: { state: 'uploaded' } }])
    renderUI(<Backups instance={instance} user={operator} />); await screen.findByText('Before changes')
    expect(screen.getByRole('button', { name: 'Create backup' })).toBeEnabled()
    expect(screen.queryByRole('link', { name: /Download backup/ })).not.toBeInTheDocument()
    expect(screen.queryByRole('button', { name: 'Restore preview' })).not.toBeInTheDocument()
    expect(screen.queryByRole('button', { name: 'Fetch verified offsite copy' })).not.toBeInTheDocument()
    expect(screen.getByRole('button', { name: 'Verify' })).toHaveProperty('disabled', !local)
  })
  it('reports fetch failures without enabling restore or verification', async () => {
    mockAPI((path, init) => init.method === 'POST' ? json({ error: 'The remote archive checksum did not match.' }, 409) : [{ ...backup, local: false, offsite: { state: 'uploaded' } }])
    const user = userEvent.setup(); renderUI(<Backups instance={instance} user={admin} />)
    await user.click(await screen.findByRole('button', { name: 'Fetch verified offsite copy' }))
    expect(await screen.findByText('The remote archive checksum did not match.')).toBeVisible()
    expect(screen.getByRole('button', { name: 'Restore preview' })).toBeDisabled()
    expect(screen.getByRole('button', { name: 'Verify' })).toBeDisabled()
  })
  it('does not count a trusted remote-only catalog entry as a verified local restore point', async () => {
    mockAPI(path => path.endsWith('/backups') ? [{ ...backup, local: false, offsite: { state: 'uploaded' } }] : path.endsWith('/status') ? { state: 'offline', ready: false, players: [], uptime_seconds: 0, memory_bytes: 0, cpu_seconds: 0 } : [])
    renderUI(<Dashboard instance={instance} user={owner} />)
    expect(await screen.findByText('No local restore point has recorded creation verification.')).toBeVisible()
  })
})
