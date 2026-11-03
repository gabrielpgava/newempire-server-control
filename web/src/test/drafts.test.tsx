import { act, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it, vi } from 'vitest'
import { App } from '../App'
import { getCSRFToken, type Instance } from '../api'
import { instance, job, json, mockAPI, renderUI, session } from './helpers'

const filesRoute = `#/instances/${instance.id}/files`
const propertiesPath = `/instances/${instance.id}/properties`
const secondInstance = { ...instance, id: 'd'.repeat(32), name: 'Second server' }

function appAPI(instances: Instance[] = [instance, secondInstance]) {
  const failures = new Map<string, number>()
  const api = mockAPI((path, init) => {
    const status = failures.get(path)
    if (status) return json({ error: `Unavailable: ${path}` }, status)
    if (path === '/setup') return { required: false }
    if (path === '/auth/session') return session
    if (path === '/instances') return instances
    if (path === propertiesPath) return { properties: { motd: 'Original message' }, restart_required: false }
    if (path.includes('preview=true')) return { path: 'config.txt', content: 'original', etag: 'revision-1' }
    if (path.includes('/files?')) return [{ name: 'config.txt', path: 'config.txt', size: 8, is_dir: false }]
    if (path.endsWith('/files') && init.method === 'POST') return job
    if (path === '/jobs' || path.endsWith('/backups')) return []
    if (path.endsWith('/status')) return { state: 'stopped', ready: false, uptime_seconds: 0, memory_bytes: 0, cpu_seconds: 0, players: [] }
    if (path.startsWith('/fabric/versions')) return { game_versions: ['test-game'], loader_versions: ['test-loader'], installer_versions: ['test-installer'] }
    if (path === '/java') return [{ path: '/usr/bin/java', version: 'test-java', major: 99 }]
    return json({ error: `Unexpected fixture request: ${path}` }, 404)
  })
  return { failures, api }
}

function openApp(route = filesRoute) {
  window.history.replaceState(null, '', route)
  return renderUI(<App />)
}

async function editFile(user: ReturnType<typeof userEvent.setup>) {
  await user.click(await screen.findByRole('button', { name: 'config.txt' }))
  const editor = await screen.findByLabelText('File content')
  await user.type(editor, '-draft')
  return editor
}

function beforeUnload() {
  const event = new Event('beforeunload', { cancelable: true })
  window.dispatchEvent(event)
  return event
}

describe('drafts survive failed background refreshes', () => {
  it.each(['/instances', propertiesPath, '/setup', '/auth/session'])('keeps an edited field mounted when %s fails, including during retry', async path => {
    const { failures } = appAPI(); const user = userEvent.setup()
    const { client } = openApp(`#/instances/${instance.id}/settings`)
    const field = await screen.findByLabelText('motd')
    await user.clear(field); await user.type(field, 'Local draft')
    failures.set(path, 503)
    await act(async () => { await client.refetchQueries({ queryKey: [path], exact: true }) })
    expect(await screen.findByText(`Unavailable: ${path}`)).toBeVisible()
    expect(screen.getByText('Showing previously loaded data; it may be out of date.')).toBeVisible()
    expect(screen.getByLabelText('motd')).toBe(field)
    expect(field).toHaveValue('Local draft'); expect(field).toHaveFocus()
    failures.delete(path)
    await user.click(screen.getByRole('button', { name: 'Try again' }))
    await waitFor(() => expect(screen.queryByText(`Unavailable: ${path}`)).not.toBeInTheDocument())
    expect(screen.getByLabelText('motd')).toBe(field); expect(field).toHaveValue('Local draft')
  })

  it('blocks an initial instance-list error until retry succeeds', async () => {
    const { failures } = appAPI(); failures.set('/instances', 503)
    const user = userEvent.setup(); openApp()
    expect(await screen.findByText('Unavailable: /instances')).toBeVisible()
    expect(screen.queryByRole('heading', { name: 'Files' })).not.toBeInTheDocument()
    expect(screen.queryByText('Showing previously loaded data; it may be out of date.')).not.toBeInTheDocument()
    failures.clear(); await user.click(screen.getByRole('button', { name: 'Try again' }))
    expect(await screen.findByRole('button', { name: 'config.txt' })).toBeVisible()
  })

  it('keeps a failed initial page query blocking rather than rendering a form with invented data', async () => {
    const { failures } = appAPI(); failures.set(propertiesPath, 503)
    openApp(`#/instances/${instance.id}/settings`)
    expect(await screen.findByText(`Unavailable: ${propertiesPath}`)).toBeVisible()
    expect(screen.queryByLabelText('motd')).not.toBeInTheDocument()
    expect(screen.queryByText('Showing previously loaded data; it may be out of date.')).not.toBeInTheDocument()
  })

  it('treats a cached empty instance array as loaded data and preserves the first-run draft', async () => {
    const { failures } = appAPI([]); const user = userEvent.setup(); const { client } = openApp('#/')
    const field = await screen.findByLabelText('Instance name'); await user.type(field, 'My draft server')
    failures.set('/instances', 503)
    await act(async () => { await client.refetchQueries({ queryKey: ['/instances'], exact: true }) })
    expect(await screen.findByText('Unavailable: /instances')).toBeVisible()
    expect(screen.getByText('Showing previously loaded data; it may be out of date.')).toBeVisible()
    expect(screen.getByLabelText('Instance name')).toBe(field); expect(field).toHaveValue('My draft server')
  })

  it.each(['/instances', '/auth/session'])('clears protected data and the editor when %s returns an expired session', async path => {
    const { failures } = appAPI(); const user = userEvent.setup(); const { client } = openApp()
    await editFile(user); failures.set(path, 401)
    await act(async () => { await client.refetchQueries({ queryKey: [path], exact: true }) })
    expect(await screen.findByRole('heading', { name: 'Welcome back' })).toBeVisible()
    expect(screen.queryByLabelText('File content')).not.toBeInTheDocument()
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument()
    expect(getCSRFToken()).toBe('')
    expect(client.getQueryData(['/instances'])).toBeUndefined()
    expect(beforeUnload().defaultPrevented).toBe(false)
  })
})

describe('file draft navigation protection in the actual shell', () => {
  it('cancels shell navigation without changing the URL, draft, or focus target, then confirms and cleans up', async () => {
    appAPI(); const user = userEvent.setup(); openApp(); const editor = await editFile(user)
    const link = screen.getByRole('link', { name: 'Jobs' })
    await user.click(link)
    const dialog = await screen.findByRole('dialog', { name: 'Discard unsaved text?' })
    await waitFor(() => expect(window.location.hash).toBe(filesRoute))
    expect(screen.getByLabelText('File content')).toBe(editor); expect(editor).toHaveValue('original-draft')
    await user.click(within(dialog).getByText('Cancel', { selector: 'button' }))
    expect(window.location.hash).toBe(filesRoute); expect(editor).toHaveValue('original-draft')
    expect(link).toHaveFocus()
    await user.click(link)
    await user.click(await screen.findByRole('button', { name: 'Discard draft' }))
    expect(await screen.findByRole('heading', { name: 'Jobs' })).toBeVisible()
    expect(window.location.hash).toBe('#/jobs'); expect(beforeUnload().defaultPrevented).toBe(false)
    await user.click(screen.getByRole('link', { name: 'Files' }))
    expect(await screen.findByRole('heading', { name: 'Files' })).toBeVisible()
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument()
    expect(screen.queryByLabelText('File content')).not.toBeInTheDocument()
  })

  it('requires the same confirmation for instance changes and keeps the original selection on cancel', async () => {
    appAPI(); const user = userEvent.setup(); openApp(); const editor = await editFile(user)
    const selector = screen.getByRole('combobox', { name: 'Selected instance' })
    await user.selectOptions(selector, secondInstance.id)
    const dialog = await screen.findByRole('dialog', { name: 'Discard unsaved text?' })
    await waitFor(() => expect(window.location.hash).toBe(filesRoute))
    await user.click(within(dialog).getByText('Cancel', { selector: 'button' }))
    expect(selector).toHaveValue(instance.id); expect(selector).toHaveFocus()
    expect(editor).toHaveValue('original-draft')
    await user.selectOptions(selector, secondInstance.id)
    await user.click(await screen.findByRole('button', { name: 'Discard draft' }))
    await waitFor(() => expect(selector).toHaveValue(secondInstance.id))
    expect(window.location.hash).toBe(`#/instances/${secondInstance.id}/files`)
    expect(screen.queryByLabelText('File content')).not.toBeInTheDocument()
    expect(beforeUnload().defaultPrevented).toBe(false)
  })

  it('guards direct hash changes, restores the editor focus on cancel, and does not strand later navigation', async () => {
    appAPI(); const user = userEvent.setup(); openApp(); const editor = await editFile(user)
    act(() => { window.location.hash = '#/jobs' })
    const dialog = await screen.findByRole('dialog', { name: 'Discard unsaved text?' })
    await waitFor(() => expect(window.location.hash).toBe(filesRoute))
    await user.click(within(dialog).getByText('Cancel', { selector: 'button' }))
    expect(window.location.hash).toBe(filesRoute); expect(editor).toHaveValue('original-draft'); expect(editor).toHaveFocus()
    act(() => { window.location.hash = '#/jobs' })
    await user.click(await screen.findByRole('button', { name: 'Discard draft' }))
    expect(await screen.findByRole('heading', { name: 'Jobs' })).toBeVisible()
    act(() => { window.location.hash = filesRoute })
    expect(await screen.findByRole('button', { name: 'config.txt' })).toBeVisible()
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument()
  })

  it.each(['back', 'forward'] as const)('preserves the %s history destination after cancel and allows it after confirmation', async direction => {
    appAPI(); const user = userEvent.setup(); openApp('#/jobs')
    await user.click(await screen.findByRole('link', { name: 'Files' }))
    await screen.findByRole('button', { name: 'config.txt' })
    if (direction === 'forward') {
      await user.click(screen.getByRole('link', { name: 'Jobs' }))
      await screen.findByRole('heading', { name: 'Jobs' })
      act(() => { window.history.back() })
      await screen.findByRole('button', { name: 'config.txt' })
    }
    const editor = await editFile(user)
    act(() => { window.history[direction]() })
    const dialog = await screen.findByRole('dialog', { name: 'Discard unsaved text?' })
    await waitFor(() => expect(window.location.hash).toBe(filesRoute))
    await user.click(within(dialog).getByText('Cancel', { selector: 'button' }))
    expect(editor).toHaveValue('original-draft'); expect(editor).toHaveFocus()
    act(() => { window.history[direction]() })
    await user.click(await screen.findByRole('button', { name: 'Discard draft' }))
    expect(await screen.findByRole('heading', { name: 'Jobs' })).toBeVisible()
    expect(window.location.hash).toBe('#/jobs'); expect(beforeUnload().defaultPrevented).toBe(false)
  })

  it('registers unload protection only while an unqueued local draft exists and removes the exact listener on cleanup', async () => {
    const add = vi.spyOn(window, 'addEventListener'), remove = vi.spyOn(window, 'removeEventListener')
    appAPI(); const user = userEvent.setup(); const { unmount } = openApp()
    await user.click(await screen.findByRole('button', { name: 'config.txt' }))
    const editor = await screen.findByLabelText('File content')
    expect(add.mock.calls.filter(([name]) => name === 'beforeunload')).toHaveLength(0)
    await user.type(editor, '-draft')
    const handler = add.mock.calls.find(([name]) => name === 'beforeunload')?.[1]
    expect(handler).toBeTypeOf('function'); expect(beforeUnload().defaultPrevented).toBe(true)
    await user.clear(editor); await user.type(editor, 'original')
    expect(remove).toHaveBeenCalledWith('beforeunload', handler)
    expect(beforeUnload().defaultPrevented).toBe(false)
    await user.type(editor, '-draft'); expect(beforeUnload().defaultPrevented).toBe(true)
    unmount(); expect(beforeUnload().defaultPrevented).toBe(false)
  })

  it('allows leaving an accepted queued edit without a discard or unload prompt', async () => {
    appAPI(); const user = userEvent.setup(); openApp(); await editFile(user)
    await user.click(screen.getByRole('button', { name: 'Queue text edit' }))
    await user.click(screen.getByRole('button', { name: 'Apply change' }))
    expect(await screen.findByText('Edit queued; follow the job before reloading.')).toBeVisible()
    expect(beforeUnload().defaultPrevented).toBe(false)
    await user.click(screen.getByRole('link', { name: 'Jobs' }))
    expect(await screen.findByRole('heading', { name: 'Jobs' })).toBeVisible()
    expect(screen.queryByRole('button', { name: 'Discard draft' })).not.toBeInTheDocument()
  })

  it('keeps both guards after a rejected queue request, and protects sign-out before sending it', async () => {
    const { failures, api } = appAPI(); failures.set(`/instances/${instance.id}/files`, 409)
    const user = userEvent.setup(); openApp(); const editor = await editFile(user)
    await user.click(screen.getByRole('button', { name: 'Queue text edit' }))
    await user.click(screen.getByRole('button', { name: 'Apply change' }))
    expect(await screen.findByText(`Unavailable: /instances/${instance.id}/files`)).toBeVisible()
    expect(beforeUnload().defaultPrevented).toBe(true)
    await user.click(screen.getByRole('button', { name: 'Sign out' }))
    const dialog = await screen.findByRole('dialog', { name: 'Discard unsaved text?' })
    await user.click(within(dialog).getByText('Cancel', { selector: 'button' }))
    expect(editor).toHaveValue('original-draft'); expect(window.location.hash).toBe(filesRoute)
    expect(api.mock.calls.some(([url]) => String(url).endsWith('/auth/logout'))).toBe(false)
    expect(beforeUnload().defaultPrevented).toBe(true)
  })
})
