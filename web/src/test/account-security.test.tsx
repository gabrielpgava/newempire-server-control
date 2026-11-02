import { screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it, vi } from 'vitest'
import { getCSRFToken, type User } from '../api'
import { Settings } from '../pages/Settings'
import { instance, json, mockAPI, owner, renderUI, viewer } from './helpers'

function section(title: string) { return within(screen.getByRole('heading', { name: title }).closest('section')!) }
async function fillPassword(user: ReturnType<typeof userEvent.setup>, next: string, confirmation = next) {
  const fields = section('Change password')
  await user.type(fields.getByLabelText('Current password'), 'current-password')
  await user.type(fields.getByLabelText('New password'), next)
  await user.type(fields.getByLabelText('Confirm new password'), confirmation)
  await user.click(fields.getByRole('button', { name: 'Change password' }))
}
async function fillDisable(user: ReturnType<typeof userEvent.setup>, code = '123456') {
  const fields = section('Account security')
  await user.type(fields.getByLabelText('Current password'), 'current-password')
  await user.type(fields.getByLabelText('Six-digit authenticator code'), code)
  await user.click(fields.getByRole('button', { name: 'Disable two-factor authentication' }))
}

describe('owner user revocation', () => {
  it('protects self/owner rows and revokes a non-owner only after typed confirmation', async () => {
    const otherOwner: User = { ...owner, id: 'other-owner', username: 'another-owner' }
    let records = [owner, otherOwner, { ...viewer, two_factor: true }]
    const writes: { path: string; method: string; body: unknown }[] = []
    mockAPI((path, init) => {
      if (init.method === 'DELETE') { writes.push({ path, method: init.method, body: JSON.parse(String(init.body)) }); records = records.filter(user => user.id !== viewer.id); return { ok: true } }
      return records
    })
    const user = userEvent.setup(); renderUI(<Settings user={owner} instances={[instance]} />)
    await user.click(screen.getByRole('button', { name: 'Users & access' })); await screen.findByText('another-owner')
    for (const name of [owner.username, otherOwner.username]) {
      const row = screen.getByText(name, { selector: 'strong' }).closest('tr')!
      expect(within(row).queryByRole('button', { name: /Revoke user/ })).not.toBeInTheDocument()
    }
    expect(screen.queryByRole('button', { name: /Disable two-factor/ })).not.toBeInTheDocument()
    await user.click(screen.getByRole('button', { name: `Revoke user ${viewer.username}` }))
    expect(writes).toHaveLength(0)
    expect(screen.getByText(/All sessions and instance access for this user will be revoked/)).toBeVisible()
    const dialog = within(screen.getByRole('dialog'))
    expect(dialog.getByRole('button', { name: 'Revoke user' })).toBeDisabled()
    await user.type(dialog.getByLabelText(/Type “reader”/), viewer.username)
    await user.click(dialog.getByRole('button', { name: 'Revoke user' }))
    await waitFor(() => expect(writes).toEqual([{ path: `/users/${viewer.id}`, method: 'DELETE', body: {} }]))
    expect(await screen.findByText('User access revoked. All sessions for that account have been invalidated.')).toBeVisible()
    await waitFor(() => expect(screen.queryByRole('button', { name: `Revoke user ${viewer.username}` })).not.toBeInTheDocument())
  })
  it('does not revoke after cancelling the protected action', async () => {
    const writes: string[] = []
    mockAPI((path, init) => { if (init.method) writes.push(path); return [owner, viewer] })
    const user = userEvent.setup(); renderUI(<Settings user={owner} instances={[]} />)
    await user.click(screen.getByRole('button', { name: 'Users & access' }))
    await user.click(await screen.findByRole('button', { name: `Revoke user ${viewer.username}` }))
    await user.click(within(screen.getByRole('dialog')).getByText('Cancel', { selector: 'button' }))
    expect(writes).toHaveLength(0)
  })
  it('preserves the user row and shows a rejected revocation without claiming success', async () => {
    mockAPI((path, init) => init.method === 'DELETE' ? json({ error: 'This account cannot be revoked.' }, 409) : [owner, viewer])
    const user = userEvent.setup(); renderUI(<Settings user={owner} instances={[]} />)
    await user.click(screen.getByRole('button', { name: 'Users & access' }))
    await user.click(await screen.findByRole('button', { name: `Revoke user ${viewer.username}` }))
    const dialog = within(screen.getByRole('dialog')); await user.type(dialog.getByLabelText(/Type “reader”/), viewer.username)
    await user.click(dialog.getByRole('button', { name: 'Revoke user' }))
    expect(await screen.findByText('This account cannot be revoked.')).toBeVisible()
    expect(screen.getByRole('button', { name: `Revoke user ${viewer.username}` })).toBeVisible()
    expect(screen.queryByText(/User access revoked/)).not.toBeInTheDocument()
  })
  it.each(['admin', 'operator', 'viewer'] as const)('does not expose user management to %s', role => {
    const api = mockAPI(() => []); renderUI(<Settings user={{ ...viewer, role }} instances={[instance]} />)
    expect(screen.queryByRole('button', { name: 'Users & access' })).not.toBeInTheDocument()
    expect(screen.queryByRole('button', { name: /Revoke user/ })).not.toBeInTheDocument()
    expect(api).not.toHaveBeenCalled()
    expect(screen.getByRole('button', { name: 'Change password' })).toBeVisible()
  })
})

describe('self-service password change', () => {
  it('posts only current/new passwords after confirmation, then clears all cached session data', async () => {
    const writes: { path: string; method: string | undefined; body: unknown }[] = []
    mockAPI((path, init) => { writes.push({ path, method: init.method, body: JSON.parse(String(init.body)) }); return { ok: true } })
    const user = userEvent.setup(); const { client } = renderUI(<Settings user={viewer} instances={[]} />)
    client.setQueryData(['private-session-data'], { secret: 'must-not-survive' }); const clear = vi.spyOn(client, 'clear')
    await fillPassword(user, 'é'.repeat(6))
    expect(writes).toHaveLength(0)
    expect(screen.getByText(/All current sessions for your account will be revoked/)).toBeVisible()
    await user.click(within(screen.getByRole('dialog')).getByRole('button', { name: 'Change password and sign out' }))
    await waitFor(() => expect(writes).toEqual([{ path: '/auth/password', method: 'POST', body: { current_password: 'current-password', new_password: 'é'.repeat(6) } }]))
    await waitFor(() => expect(clear).toHaveBeenCalledOnce())
    expect(getCSRFToken()).toBe(''); expect(client.getQueryCache().getAll()).toHaveLength(0)
    expect(section('Change password').getByLabelText('New password')).toHaveValue('')
  })
  it.each([
    { next: 'short123', confirmation: 'short123', error: 'Use a new password of at least 12 UTF-8 bytes.' },
    { next: 'new-strong-password', confirmation: 'different-strong-password', error: 'The new passwords do not match.' },
  ])('rejects invalid local password confirmation: $error', async ({ next, confirmation, error }) => {
    const api = mockAPI(() => ({ ok: true })); const user = userEvent.setup(); renderUI(<Settings user={viewer} instances={[]} />)
    await fillPassword(user, next, confirmation)
    expect(await screen.findByText(error)).toBeVisible(); expect(api).not.toHaveBeenCalled()
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument()
    expect(getCSRFToken()).toBe('csrf-test-token')
  })
  it('does not change the password if the session-revocation warning is cancelled', async () => {
    const api = mockAPI(() => ({ ok: true })); const user = userEvent.setup(); renderUI(<Settings user={owner} instances={[]} />)
    await fillPassword(user, 'new-strong-password')
    await user.click(within(screen.getByRole('dialog')).getByText('Cancel', { selector: 'button' }))
    expect(api).not.toHaveBeenCalled(); expect(getCSRFToken()).toBe('csrf-test-token')
  })
  it('keeps the current session on a rejected password change', async () => {
    mockAPI(() => json({ error: 'The current password is incorrect.' }, 403))
    const user = userEvent.setup(); const { client } = renderUI(<Settings user={owner} instances={[]} />); const clear = vi.spyOn(client, 'clear')
    await fillPassword(user, 'new-strong-password')
    await user.click(within(screen.getByRole('dialog')).getByRole('button', { name: 'Change password and sign out' }))
    expect(await screen.findByText('The current password is incorrect.')).toBeVisible()
    expect(clear).not.toHaveBeenCalled(); expect(getCSRFToken()).toBe('csrf-test-token')
  })
})

describe('self-service two-factor disable', () => {
  const enabledUser = { ...viewer, two_factor: true }
  it('requires confirmation and sends a password plus code to DELETE, then clears the session', async () => {
    const writes: { path: string; method: string | undefined; body: unknown }[] = []
    mockAPI((path, init) => { writes.push({ path, method: init.method, body: JSON.parse(String(init.body)) }); return { ok: true } })
    const user = userEvent.setup(); const { client } = renderUI(<Settings user={enabledUser} instances={[]} />)
    client.setQueryData(['private-session-data'], { cached: true }); const clear = vi.spyOn(client, 'clear')
    expect(screen.getByText(/removes the additional sign-in protection and revokes all current sessions/)).toBeVisible()
    await fillDisable(user); expect(writes).toHaveLength(0)
    await user.click(within(screen.getByRole('dialog')).getByRole('button', { name: 'Disable two-factor and sign out' }))
    await waitFor(() => expect(writes).toEqual([{ path: '/auth/totp', method: 'DELETE', body: { password: 'current-password', code: '123456' } }]))
    await waitFor(() => expect(clear).toHaveBeenCalledOnce())
    expect(getCSRFToken()).toBe(''); expect(client.getQueryCache().getAll()).toHaveLength(0)
    expect(section('Account security').getByLabelText('Current password')).toHaveValue('')
  })
  it('does not submit an incomplete code or a cancelled confirmation', async () => {
    const api = mockAPI(() => ({ ok: true })); const user = userEvent.setup(); renderUI(<Settings user={enabledUser} instances={[]} />)
    await fillDisable(user, '12345'); expect(api).not.toHaveBeenCalled(); expect(screen.queryByRole('dialog')).not.toBeInTheDocument()
    await user.type(section('Account security').getByLabelText('Six-digit authenticator code'), '6')
    await user.click(screen.getByRole('button', { name: 'Disable two-factor authentication' }))
    await user.click(within(screen.getByRole('dialog')).getByText('Cancel', { selector: 'button' }))
    expect(api).not.toHaveBeenCalled(); expect(getCSRFToken()).toBe('csrf-test-token')
  })
  it('does not clear the session if the code is rejected', async () => {
    mockAPI(() => json({ error: 'The authenticator code is invalid.' }, 400))
    const user = userEvent.setup(); const { client } = renderUI(<Settings user={enabledUser} instances={[]} />); const clear = vi.spyOn(client, 'clear')
    await fillDisable(user); await user.click(within(screen.getByRole('dialog')).getByRole('button', { name: 'Disable two-factor and sign out' }))
    expect(await screen.findByText('The authenticator code is invalid.')).toBeVisible()
    expect(clear).not.toHaveBeenCalled(); expect(getCSRFToken()).toBe('csrf-test-token')
  })
  it('keeps enrollment on the same session-revocation flow without adding any user-id override', async () => {
    const writes: unknown[] = []
    mockAPI((path, init) => { const body = JSON.parse(String(init.body)); writes.push({ path, method: init.method, body }); return body.secret ? { ok: true } : { secret: 'new-totp-secret' } })
    const user = userEvent.setup(); const { client } = renderUI(<Settings user={viewer} instances={[]} />); const clear = vi.spyOn(client, 'clear')
    await user.click(screen.getByRole('button', { name: 'Generate authenticator secret' }))
    await screen.findByLabelText('Authenticator secret')
    const fields = section('Account security')
    await user.type(fields.getByLabelText('Current password'), 'current-password'); await user.type(fields.getByLabelText('Six-digit authenticator code'), '123456')
    await user.click(fields.getByRole('button', { name: 'Enable two-factor and sign in again' }))
    await waitFor(() => expect(clear).toHaveBeenCalledOnce())
    expect(writes).toEqual([{ path: '/auth/totp', method: 'POST', body: {} }, { path: '/auth/totp', method: 'POST', body: { password: 'current-password', secret: 'new-totp-secret', code: '123456' } }])
    expect(getCSRFToken()).toBe(''); expect(screen.queryByLabelText('Authenticator secret')).not.toBeInTheDocument()
  })
})
