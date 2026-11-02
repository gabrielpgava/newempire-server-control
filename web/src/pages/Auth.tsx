import { useState } from 'react'
import { KeyRound, ShieldCheck } from 'lucide-react'
import { useQueryClient } from '@tanstack/react-query'
import { APIError, request, setSession, type Session } from '../api'
import { Button, ErrorNotice, Field, SubmitForm } from '../components'

export function Auth({ setup }: { setup: boolean }) {
  const client = useQueryClient()
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<unknown>(null)
  async function submit(data: FormData) {
    setBusy(true); setError(null)
    try {
      const password = String(data.get('password'))
      if (setup && password !== data.get('password_confirmation')) throw new APIError('The passwords do not match.', 400)
      await request(setup ? '/setup' : '/auth/login', { method: 'POST', body: JSON.stringify({ username: data.get('username'), password, ...(setup ? { token: data.get('token') } : { code: data.get('code') || undefined }) }) })
      const session = await request<Session>('/auth/session')
      setSession(session)
      client.setQueryData(['/auth/session'], session)
      client.setQueryData(['/setup'], { required: false })
      await client.invalidateQueries()
    } catch (cause) { setError(cause) } finally { setBusy(false) }
  }
  return <main className="auth-page"><div className="auth-identity"><div className="brand-mark" aria-hidden="true"><ShieldCheck size={26} /></div><strong>New Empire<span>Server Control</span></strong></div><section className="auth-form"><h1>{setup ? 'Set up your control panel' : 'Welcome back'}</h1><p>{setup ? 'Create the owner account. Your setup token is available on the panel host.' : 'Sign in to manage your Minecraft instances.'}</p>{!!error && <ErrorNotice error={error} />}<SubmitForm onSubmit={submit}>
    {setup && <Field label="Setup token" hint="Use the one-time token printed by the panel during first startup."><input name="token" autoComplete="off" required autoFocus type="password" /></Field>}
    <Field label="Username"><input name="username" autoComplete="username" required minLength={3} maxLength={64} autoFocus={!setup} /></Field>
    <Field label="Password" hint={setup ? 'Use at least 12 characters and a unique password.' : undefined}><input name="password" type="password" autoComplete={setup ? 'new-password' : 'current-password'} required minLength={setup ? 12 : undefined} maxLength={1024} /></Field>
    {setup && <Field label="Confirm password"><input name="password_confirmation" type="password" autoComplete="new-password" required minLength={12} /></Field>}
    {!setup && <Field label="Two-factor code (if enabled)"><input name="code" inputMode="numeric" autoComplete="one-time-code" pattern="[0-9]{6}" maxLength={6} /></Field>}
    <Button type="submit" variant="primary" busy={busy}><KeyRound size={16} />{setup ? 'Create owner account' : 'Sign in'}</Button>
  </SubmitForm></section><p className="auth-footer">Worlds matter. Changes should be deliberate.</p></main>
}
