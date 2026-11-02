import { screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it, vi } from 'vitest'
import type { UploadOptions } from 'tus-js-client'
import { Mods } from '../pages/Mods'
import { Players } from '../pages/Players'
import { Backups } from '../pages/Backups'
import { instance, job, json, mockAPI, owner, renderUI, viewer } from './helpers'

const uploadID = 'c'.repeat(32)
vi.mock('tus-js-client', () => ({
  Upload: class {
    url = `${window.location.origin}/api/v1/uploads/${'c'.repeat(32)}`
    constructor(private file: File, private options: UploadOptions) {}
    async findPreviousUploads() { return [] }
    start() { this.options.onProgress?.(this.file.size, this.file.size); this.options.onSuccess?.({ lastResponse: { getStatus: () => 204, getHeader: () => undefined, getBody: () => '', getUnderlyingObject: () => null } }) }
    async abort() {}
  },
}))

const modPlan = { plan_hash: 'reviewed-pack-hash', actions: [{ project_id: 'project-a', name: 'Server content', from_version: '1', to_version: '2', filename: 'server-mod.jar', reason: 'Server pack version' }], blocked: [] as string[], warnings: ['Review the server-side overrides.'] }
const emptyPlayers = { online: [], whitelist: [], operators: [], bans: [] }
function modAPI(preview: unknown = modPlan) {
  const calls: { path: string; body: unknown }[] = []
  mockAPI((path, init) => {
    if (init.method === 'POST') calls.push({ path, body: init.body ? JSON.parse(String(init.body)) : undefined })
    if (path.endsWith('/modpacks/preview')) return preview
    if (path.endsWith('/modpacks/apply')) return job
    if (path.endsWith('/mods/policy')) return { mode: 'manual', interval_hours: 24, window_start: '04:00', window_end: '05:00', timezone: 'UTC', postpone_players: true }
    return []
  })
  return calls
}
async function stagePack(user: ReturnType<typeof userEvent.setup>) {
  await user.upload(screen.getByLabelText('Choose a server .mrpack file'), new File(['test archive'], 'server.mrpack', { type: 'application/octet-stream' }))
  await user.click(screen.getByRole('button', { name: 'Upload to staging' }))
  await user.click(await screen.findByRole('button', { name: 'Preview server modpack' }))
}

describe('server modpack workflow', () => {
  it('stages only .mrpack, reviews the server plan, and sends the reviewed hash after confirmation', async () => {
    const calls = modAPI(); const user = userEvent.setup(); renderUI(<Mods instance={instance} user={owner} />)
    expect(screen.getByLabelText('Choose a server .mrpack file')).toHaveAttribute('accept', '.mrpack')
    expect(screen.queryByRole('radio', { name: 'Folder' })).not.toBeInTheDocument()
    expect(screen.getByRole('link', { name: 'Export .mrpack' })).toHaveAttribute('href', `/api/v1/instances/${instance.id}/modpacks/export`)
    await stagePack(user)
    expect(await screen.findByText('Server content')).toBeVisible()
    expect(screen.getByText('Review the server-side overrides.')).toBeVisible()
    expect(calls).toEqual([{ path: `/instances/${instance.id}/modpacks/preview`, body: { upload_id: uploadID } }])
    await user.click(screen.getByRole('button', { name: 'Apply reviewed server modpack' }))
    expect(calls).toHaveLength(1)
    await user.type(screen.getByLabelText(/Type “Test survival”/), instance.name)
    await user.click(screen.getByRole('button', { name: 'Apply server modpack' }))
    await waitFor(() => expect(calls[1]).toEqual({ path: `/instances/${instance.id}/modpacks/apply`, body: { upload_id: uploadID, plan_hash: modPlan.plan_hash, confirm: true } }))
    expect(await screen.findByText('Job queued')).toBeVisible()
  })
  it('shows blocked plans without offering an enabled apply action', async () => {
    const calls = modAPI({ ...modPlan, blocked: ['An installed version is pinned.'] }); const user = userEvent.setup()
    renderUI(<Mods instance={instance} user={owner} />); await stagePack(user)
    expect(await screen.findByText('An installed version is pinned.')).toBeVisible()
    expect(screen.getByRole('button', { name: 'Apply reviewed server modpack' })).toBeDisabled()
    expect(calls.every(call => !call.path.endsWith('/apply'))).toBe(true)
  })
  it('reports inspection failures and clears a reviewed plan when another archive is chosen', async () => {
    modAPI(); const user = userEvent.setup(); renderUI(<Mods instance={instance} user={owner} />); await stagePack(user)
    await screen.findByRole('button', { name: 'Apply reviewed server modpack' })
    await user.upload(screen.getByLabelText('Choose a server .mrpack file'), new File(['replacement'], 'replacement.mrpack'))
    expect(screen.queryByRole('button', { name: 'Apply reviewed server modpack' })).not.toBeInTheDocument()
    expect(screen.queryByRole('button', { name: 'Preview server modpack' })).not.toBeInTheDocument()
  })
  it('keeps a failed preview visible rather than reporting an installed pack', async () => {
    modAPI(json({ error: 'The modpack index contains an unsafe download.' }, 400)); const user = userEvent.setup()
    renderUI(<Mods instance={instance} user={owner} />); await stagePack(user)
    expect(await screen.findByText('The modpack index contains an unsafe download.')).toBeVisible()
    expect(screen.queryByRole('button', { name: 'Apply reviewed server modpack' })).not.toBeInTheDocument()
    expect(screen.queryByText('Job queued')).not.toBeInTheDocument()
  })
  it('rejects a non-mrpack source even if the browser accept filter is bypassed', async () => {
    modAPI(); const user = userEvent.setup({ applyAccept: false }); renderUI(<Mods instance={instance} user={owner} />)
    await user.upload(screen.getByLabelText('Choose a server .mrpack file'), new File(['zip'], 'wrong.zip'))
    await user.click(screen.getByRole('button', { name: 'Upload to staging' }))
    expect(await screen.findByText(/Choose a server modpack with the .mrpack extension/)).toBeVisible()
    expect(screen.queryByRole('button', { name: 'Preview server modpack' })).not.toBeInTheDocument()
  })
  it.each(['operator', 'viewer'] as const)('hides modpack import and export for %s roles', role => {
    modAPI(); renderUI(<Mods instance={instance} user={{ ...viewer, role }} />)
    expect(screen.queryByRole('heading', { name: 'Server modpack' })).not.toBeInTheDocument()
    expect(screen.queryByRole('link', { name: 'Export .mrpack' })).not.toBeInTheDocument()
  })
})

describe('offline operator level and backup wording', () => {
  it('requires confirmation and sends the existing operator name and chosen level', async () => {
    let body: unknown
    mockAPI((path, init) => { if (init.method === 'POST') { body = JSON.parse(String(init.body)); return job }; return emptyPlayers })
    const user = userEvent.setup(); renderUI(<Players instance={instance} user={owner} />)
    await user.selectOptions(screen.getByLabelText('Action'), 'op_level')
    expect(screen.getByText(/The server must already be stopped/)).toBeVisible()
    await user.type(screen.getByLabelText('Player name'), 'ExistingAdmin')
    await user.selectOptions(screen.getByLabelText('Operator level'), '3')
    await user.click(screen.getByRole('button', { name: 'Queue player action' })); expect(body).toBeUndefined()
    await user.click(screen.getByRole('button', { name: 'Confirm player action' }))
    await waitFor(() => expect(body).toEqual({ action: 'op_level', player: 'ExistingAdmin', value: '3' }))
    expect(await screen.findByText('Job queued')).toBeVisible()
  })
  it.each(['operator', 'viewer'] as const)('does not expose operator-level changes to a %s', role => {
    mockAPI(() => emptyPlayers); renderUI(<Players instance={instance} user={{ ...viewer, role }} />)
    expect(screen.queryByRole('option', { name: 'Set existing operator level (offline)' })).not.toBeInTheDocument()
  })
  it('qualifies stored verification and leaves current verification as a job', async () => {
    let verificationPath: string | undefined
    mockAPI((path, init) => { if (init.method === 'POST') { verificationPath = path; return job }; return [{ id: 'backup-id', instance_id: instance.id, created_at: job.created_at, size: 10, sha256: 'expected-hash', note: '', verified: true }] })
    const user = userEvent.setup(); renderUI(<Backups instance={instance} user={owner} />)
    expect(await screen.findByText('Verified when created')).toHaveClass('status-good')
    await user.click(screen.getByRole('button', { name: 'Verify' }))
    await waitFor(() => expect(verificationPath).toBe(`/instances/${instance.id}/backups/backup-id/verify`))
    expect(await screen.findByText('Job queued')).toBeVisible()
  })
})
