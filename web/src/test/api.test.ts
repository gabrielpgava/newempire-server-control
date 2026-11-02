import { beforeEach, describe, expect, it, vi } from 'vitest'
import { APIError, canAdmin, canOperate, formatBytes, formatDate, request, setSession, validRelativePath } from '../api'
import { parseRoute } from '../App'
import { json, owner, session, viewer } from './helpers'

beforeEach(() => setSession())
describe('same-origin API client', () => {
  it('includes the session CSRF token only for mutations and sends cookies', async () => {
    const fetcher = vi.fn().mockImplementation(() => Promise.resolve(json({ ok: true })))
    vi.stubGlobal('fetch', fetcher); setSession(session)
    await request('/instances', { method: 'POST', body: '{}' })
    const init = fetcher.mock.calls[0][1] as RequestInit
    expect(init.credentials).toBe('same-origin')
    expect(init.redirect).toBe('error')
    expect(new Headers(init.headers).get('X-CSRF-Token')).toBe('csrf-test-token')
    await request('/instances')
    expect(new Headers(fetcher.mock.calls[1][1].headers).has('X-CSRF-Token')).toBe(false)
  })
  it('does not allow an external URL or protocol-relative path', async () => {
    await expect(request('https://attacker.test')).rejects.toThrow('API-relative')
    await expect(request('//attacker.test')).rejects.toThrow('API-relative')
  })
  it('preserves an actionable server error rather than reporting success', async () => {
    vi.stubGlobal('fetch', vi.fn().mockResolvedValue(json({ error: 'The preview is stale. Inspect it again.' }, 409)))
    await expect(request('/instances', { method: 'POST', body: '{}' })).rejects.toMatchObject({ status: 409, message: 'The preview is stale. Inspect it again.' })
  })
  it('rejects an HTML response instead of treating the SPA fallback as API data', async () => {
    vi.stubGlobal('fetch', vi.fn().mockResolvedValue(new Response('<html />', { headers: { 'Content-Type': 'text/html' } })))
    await expect(request('/jobs')).rejects.toBeInstanceOf(APIError)
  })
  it('broadcasts expiration for protected requests, but not a failed login', async () => {
    const expired = vi.fn(); window.addEventListener('session-expired', expired)
    vi.stubGlobal('fetch', vi.fn().mockImplementation(() => Promise.resolve(json({ error: 'Session expired' }, 401))))
    await request('/auth/login', { method: 'POST' }).catch(() => {})
    expect(expired).not.toHaveBeenCalled()
    await request('/jobs').catch(() => {})
    expect(expired).toHaveBeenCalledOnce(); window.removeEventListener('session-expired', expired)
  })
})
describe('permissions and safe presentation', () => {
  it('keeps viewers read-only and operators out of admin capabilities', () => {
    expect(canOperate(viewer, 'test')).toBe(false)
    expect(canAdmin({ ...owner, role: 'operator' })).toBe(false)
    expect(canOperate({ ...owner, role: 'operator', instance_ids: ['allowed'] }, 'other')).toBe(false)
    expect(canOperate({ ...owner, role: 'operator' })).toBe(false)
  })
  it('rejects traversal paths and preserves safe nested paths', () => {
    for (const value of ['../world', '/etc/passwd', 'a/../b', 'a\\b', 'a//b', 'x\0y']) expect(validRelativePath(value)).toBe(false)
    expect(validRelativePath('world/region/r.0.0.mca')).toBe(true)
  })
  it('uses reported units and both documented timestamp encodings', () => {
    expect(formatBytes(1024)).toBe('1 KiB'); expect(formatBytes(undefined)).toBe('Not reported')
    expect(formatDate(1_793_577_600)).toBe(formatDate(new Date(1_793_577_600_000).toISOString()))
    expect(formatDate('bad')).toBe('Not reported')
  })
  it('does not route malformed instance identifiers into a workspace', () => {
    expect(parseRoute('#/instances/../../files').page).toBe('not-found')
    expect(parseRoute(`#/instances/${'a'.repeat(32)}/files`)).toMatchObject({ page: 'files', instanceID: 'a'.repeat(32) })
  })
})
