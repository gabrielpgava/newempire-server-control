import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { render } from '@testing-library/react'
import { vi } from 'vitest'
import type { ReactNode } from 'react'
import { ConfirmProvider } from '../components'
import { setSession, type Instance, type Job, type PanelSettings, type Session, type User } from '../api'

export const id = 'a'.repeat(32)
export const owner: User = { id: 'owner-id', username: 'owner', role: 'owner', two_factor: false }
export const viewer: User = { id: 'viewer-id', username: 'reader', role: 'viewer', two_factor: false }
export const session: Session = { user: owner, csrf_token: 'csrf-test-token' }
export const instance: Instance = { id, name: 'Test survival', root: `/data/${id}`, java_path: '/usr/bin/java', jar: 'fabric-server-launch.jar', minecraft_version: 'test-game', loader_version: 'test-loader', memory_mb: 2048, port: 25565, rcon_port: 25575, eula: true, adopted: false, created_at: '2026-11-02T00:00:00Z' }
export const job: Job = { id: 'b'.repeat(32), instance_id: id, kind: 'test_operation', state: 'queued', progress: 0, step: 'Waiting for the instance lock', created_at: 1_793_577_600, cancellable: true }
export const settings: PanelSettings = { public_url: 'https://panel.example.test', version: 'test-version', runtime_provider: 'systemd', adopt_roots: ['/srv/minecraft'], java_paths: ['/usr/bin/java'], storage: { configured: true, enabled: true }, notifications: { configured: true, enabled: false }, backup_retention: { enabled: true, keep_last: 10 } }

export function json(value: unknown, status = 200) { return new Response(JSON.stringify(value), { status, headers: { 'Content-Type': 'application/json' } }) }
export function mockAPI(handler: (path: string, init: RequestInit) => unknown | Response | Promise<unknown | Response>) {
  const fetcher = vi.fn(async (input: RequestInfo | URL, init: RequestInit = {}) => {
    const path = String(input).replace('/api/v1', '')
    const result = await handler(path, init)
    return result instanceof Response ? result : json(result)
  })
  vi.stubGlobal('fetch', fetcher)
  return fetcher
}
export function renderUI(children: ReactNode) {
  setSession(session)
  const client = new QueryClient({ defaultOptions: { queries: { retry: false, staleTime: Infinity }, mutations: { retry: false } } })
  const result = render(<QueryClientProvider client={client}><ConfirmProvider>{children}</ConfirmProvider></QueryClientProvider>)
  return { ...result, client }
}
