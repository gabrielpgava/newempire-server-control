import { screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it, vi } from 'vitest'
import { NewInstance } from '../pages/NewInstance'
import type { InstanceSourcePreview } from '../api'
import { job, json, mockAPI, owner, renderUI } from './helpers'

vi.mock('../uploads', () => ({
  UploadPanel: ({ onComplete }: { onComplete: (upload: { id: string; filename: string; bytes: number }) => void }) => <button onClick={() => onComplete({ id: 'upload-fixture', filename: 'server.zip', bytes: 100 })}>Stage fixture archive</button>,
}))

const reservation = 'd'.repeat(32)
const source: InstanceSourcePreview = {
  preview_hash: 'reviewed-source-hash', server_root: 'backup/server', minecraft_version: 'source-game', loader_version: 'source-loader', java_required: 23,
  files: [{ path: 'launcher.jar', size: 40 }, { path: 'Realm/level.dat', size: 60 }], file_count: 2, total_bytes: 100,
  launchers: ['launcher.jar'], worlds: ['Realm'], warnings: [],
}

function sourceAPI(preview: InstanceSourcePreview = source, major = 99) {
  const writes: { path: string; body: Record<string, unknown> }[] = []
  mockAPI((path, init) => {
    const body = init.body ? JSON.parse(String(init.body)) as Record<string, unknown> : {}
    if (init.method === 'POST') writes.push({ path, body })
    if (path.startsWith('/fabric/versions')) return { game_versions: ['source-game'], loader_versions: ['source-loader'], installer_versions: ['source-installer'] }
    if (path === '/java') return [{ path: '/java/runtime', version: `Java ${major}`, major }]
    if (path === '/adoptions') return { roots: ['/srv/external'] }
    if (path === '/adoptions/preview' || path === '/imports/preview') return preview
    if (path === '/instances') return { ...job, id: `${writes.filter(write => write.path === '/instances').length}`.padStart(32, 'b'), instance_id: reservation }
    if (path.startsWith('/jobs/')) return { job: { ...job, state: 'failed', error: 'Prepare the reserved account access' }, steps: [] }
    return []
  })
  return writes
}

async function inspectUpload(user: ReturnType<typeof userEvent.setup>) {
  await user.click(screen.getByRole('radio', { name: /Import from your browser/ }))
  await user.click(screen.getByRole('button', { name: 'Stage fixture archive' }))
  await user.click(screen.getByRole('button', { name: 'Inspect staged archive' }))
  await screen.findByDisplayValue('source-game')
}

async function completeSourceForm(user: ReturnType<typeof userEvent.setup>) {
  await user.type(screen.getByLabelText('Instance name'), 'Imported survival')
  await user.selectOptions(screen.getByLabelText('Detected server launcher'), 'launcher.jar')
  await user.selectOptions(screen.getByLabelText(/Java runtime/), '/java/runtime')
  await user.click(screen.getByRole('checkbox', { name: /Minecraft EULA/ }))
  await user.click(screen.getByRole('checkbox', { name: /reviewed the inspection/ }))
}

describe('initial server import', () => {
  it('uses the reported subtree, rebased launcher, and read-only detected versions', async () => {
    const writes = sourceAPI()
    const user = userEvent.setup()
    renderUI(<NewInstance user={owner} />)
    await inspectUpload(user)
    expect(screen.getByText('backup/server')).toBeVisible()
    expect(screen.getByLabelText('Minecraft version')).toHaveAttribute('readonly')
    expect(screen.getByLabelText('Fabric loader')).toHaveAttribute('readonly')
    expect(screen.getByText(/This source requires Java 23/)).toBeVisible()
    expect(screen.queryByRole('option', { name: 'backup/server/launcher.jar' })).not.toBeInTheDocument()
    await completeSourceForm(user)
    await user.click(screen.getByRole('button', { name: 'Validate and import server' }))
    expect(writes.some(write => write.path === '/instances')).toBe(false)
    await user.click(screen.getByRole('button', { name: 'Validate and import' }))
    await waitFor(() => expect(writes.find(write => write.path === '/instances')?.body).toMatchObject({
      mode: 'import', upload_id: 'upload-fixture', preview_hash: source.preview_hash, server_root: 'backup/server', jar: 'launcher.jar', minecraft_version: 'source-game', loader_version: 'source-loader', java_path: '/java/runtime',
    }))
    expect(writes.find(write => write.path === '/instances')?.body).not.toHaveProperty('reservation_id')
  })

  it('does not offer Java runtimes below the authoritative source minimum', async () => {
    sourceAPI(source, 17)
    const user = userEvent.setup()
    renderUI(<NewInstance user={owner} />)
    await inspectUpload(user)
    expect(screen.queryByRole('option', { name: /Java 17/ })).not.toBeInTheDocument()
    expect(screen.getByText(/No installed Java runtime meets this source's minimum/)).toBeVisible()
    expect(screen.getByRole('button', { name: 'Validate and import server' })).toBeDisabled()
  })

  it('shows ambiguous-source rejection without exposing an apply path', async () => {
    mockAPI(path => {
      if (path.startsWith('/fabric/versions')) return { game_versions: [], loader_versions: [], installer_versions: [] }
      if (path === '/java') return [{ path: '/java/runtime', version: 'Java 99', major: 99 }]
      if (path === '/imports/preview') return json({ error: 'Source has multiple supported Fabric launchers or server roots.' }, 400)
      return []
    })
    const user = userEvent.setup()
    renderUI(<NewInstance user={owner} />)
    await user.click(screen.getByRole('radio', { name: /Import from your browser/ }))
    await user.click(screen.getByRole('button', { name: 'Stage fixture archive' }))
    await user.click(screen.getByRole('button', { name: 'Inspect staged archive' }))
    expect(await screen.findByText(/Source has multiple supported/)).toBeVisible()
    expect(screen.getByRole('button', { name: 'Validate and import server' })).toBeDisabled()
  })

  it('rejects surrounding name whitespace before confirmation or enqueue', async () => {
    const writes = sourceAPI()
    const user = userEvent.setup()
    renderUI(<NewInstance user={owner} />)
    await inspectUpload(user)
    await completeSourceForm(user)
    await user.type(screen.getByLabelText('Instance name'), ' ')
    await user.click(screen.getByRole('button', { name: 'Validate and import server' }))
    expect(await screen.findByText('Remove spaces at the beginning or end of the instance name.')).toBeVisible()
    expect(screen.queryByRole('button', { name: 'Validate and import' })).not.toBeInTheDocument()
    expect(writes.some(write => write.path === '/instances')).toBe(false)
  })
})

describe('reserved directory adoption', () => {
  it('reinspects and retries an ACL failure with the same reserved identity', async () => {
    const preview = { ...source, server_root: '.', reservation_id: reservation, service_account: `nec-mc-${reservation.slice(0, 24)}` }
    const writes = sourceAPI(preview)
    const user = userEvent.setup()
    renderUI(<NewInstance user={owner} />)
    await user.click(screen.getByRole('radio', { name: /Adopt a local directory/ }))
    await user.type(screen.getByLabelText('Existing absolute directory'), '/srv/external/server')
    await user.click(await screen.findByRole('button', { name: 'Inspect directory' }))
    await screen.findByDisplayValue('source-game')
    expect(screen.getByText(preview.service_account)).toBeVisible()
    await user.click(screen.getByRole('button', { name: 'Inspect directory' }))
    await waitFor(() => expect(writes.filter(write => write.path === '/adoptions/preview')).toHaveLength(2))
    expect(writes.filter(write => write.path === '/adoptions/preview')[1].body).toEqual({ path: '/srv/external/server', reservation_id: reservation })
    await completeSourceForm(user)
    await user.click(screen.getByRole('button', { name: 'Validate and adopt directory' }))
    await user.click(screen.getByRole('button', { name: 'Validate and adopt' }))
    await user.click(await screen.findByRole('button', { name: 'Retry reserved adoption' }))
    await user.click(screen.getByRole('button', { name: 'Validate and adopt' }))
    await waitFor(() => expect(writes.filter(write => write.path === '/instances')).toHaveLength(2))
    const requests = writes.filter(write => write.path === '/instances')
    expect(requests[0].body).toEqual(requests[1].body)
    expect(requests[0].body).toMatchObject({ mode: 'adopt', path: '/srv/external/server', reservation_id: reservation, server_root: '.', minecraft_version: 'source-game', loader_version: 'source-loader', preview_hash: source.preview_hash })
    await user.type(screen.getByLabelText('Existing absolute directory'), '-other')
    expect(screen.queryByText(preview.service_account)).not.toBeInTheDocument()
    expect(screen.getByRole('button', { name: /adoption/ })).toBeDisabled()
  })
})
