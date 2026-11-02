import { useState } from 'react'
import { useQueryClient } from '@tanstack/react-query'
import { KeyRound, Save, ShieldCheck, UserPlus } from 'lucide-react'
import { canAdmin, instancePath, setSession, useAPI, useOperation, type AuditEntry, type FabricVersions, type Instance, type JavaRuntime, type PanelSettings, type Plan, type User } from '../api'
import { Button, CheckField, DataTable, DateText, Empty, ErrorNotice, Field, Notice, OperationFeedback, PageHeading, PlanView, QueryState, ReadOnly, Section, Status, SubmitForm, useConfirm } from '../components'
import { RuntimeFields } from './NewInstance'

function SavedFeedback({ operation }: { operation: { error: unknown; isSuccess: boolean } }) { return <>{!!operation.error && <ErrorNotice error={operation.error} />}{operation.isSuccess && <Notice tone="success">Settings saved.</Notice>}</> }

function Properties({ instance, user }: { instance: Instance; user: User }) {
  const properties = useAPI<{ properties: Record<string, string>; restart_required: boolean }>(instancePath(instance.id, '/properties'))
  const operation = useOperation()
  const confirm = useConfirm()
  const allowed = canAdmin(user)
  return <Section title="Server properties" description="Credentials are excluded. Unknown properties and comments are preserved by the server. The active world is managed in Worlds."><OperationFeedback operation={operation} /><QueryState query={properties}>{data => {
    const editable = Object.entries(data.properties).filter(([key]) => key !== 'level-name').sort(([a], [b]) => a.localeCompare(b))
    return <>{data.restart_required && <Notice tone="warning">Applied property changes are pending a server restart.</Notice>}<SubmitForm onSubmit={async form => {
      if (await confirm({ title: 'Apply these server properties?', body: <p>The panel validates supported keys and creates a safe configuration change. Some properties take effect only after a restart.</p>, action: 'Apply properties' })) await operation.run(instancePath(instance.id, '/properties'), { properties: Object.fromEntries(editable.map(([key]) => [key, String(form.get(key) ?? '')])), confirm: true }, 'PUT')
    }}><fieldset disabled={!allowed || operation.isPending}><div className="property-grid">{editable.map(([key, value]) => <Field key={key} label={key}>{value === 'true' || value === 'false' ? <select name={key} defaultValue={value}><option value="true">true</option><option value="false">false</option></select> : <input name={key} defaultValue={value} maxLength={2048} />}</Field>)}</div>{allowed ? <Button type="submit" variant="primary" busy={operation.isPending}><Save size={16} />Apply properties</Button> : <ReadOnly admin />}</fieldset></SubmitForm></>
  }}</QueryState></Section>
}

function Runtime({ instance, user }: { instance: Instance; user: User }) {
  const [game, setGame] = useState(instance.minecraft_version)
  const [selection, setSelection] = useState<Record<string, string>>()
  const versions = useAPI<FabricVersions>(`/fabric/versions?minecraft_version=${encodeURIComponent(game)}`, { keepPreviousData: true })
  const java = useAPI<JavaRuntime[]>('/java')
  const plan = useOperation<Plan>()
  const operation = useOperation()
  const confirm = useConfirm()
  const allowed = canAdmin(user)
  return <><Section title="Fabric and Java migration" description="Review compatibility and a transaction plan before changing runtime versions."><QueryState query={versions}>{available => <QueryState query={java} empty={<Empty title="No Java runtimes available">Configure an allowlisted Java path on the panel host.</Empty>}>{runtimes => <div onChange={() => plan.reset()}><SubmitForm onSubmit={async form => { const values = Object.fromEntries(['minecraft_version', 'loader_version', 'installer_version', 'java_path'].map(key => [key, String(form.get(key))])); setSelection(values); await plan.run(instancePath(instance.id, '/runtime/plan'), values) }}><fieldset disabled={!allowed || plan.isPending}><RuntimeFields initial={instance} versions={available} java={runtimes} onGameChange={setGame} installer />{allowed ? <Button type="submit" variant="primary" busy={plan.isPending}>Review migration plan</Button> : <ReadOnly admin />}</fieldset></SubmitForm></div>}</QueryState>}</QueryState>{plan.error && <ErrorNotice error={plan.error} />}</Section>{plan.data && <Section title="Migration plan"><PlanView plan={plan.data} />{plan.data.java_required && <p>Required Java major: <strong>{plan.data.java_required}</strong></p>}<Button variant="danger" disabled={!allowed || operation.isSuccess} busy={operation.isPending} onClick={() => { void (async () => {
    if (await confirm({ title: 'Apply this runtime migration?', body: <p>The server will create a verified restore point, shut down gracefully, and validate the unchanged plan before applying the selected runtime. World recovery is never automatic.</p>, action: 'Apply runtime migration', danger: true, verify: instance.name })) await operation.run(instancePath(instance.id, '/runtime/apply'), { ...selection, plan_hash: plan.data!.plan_hash, confirm: true }).catch(() => {})
  })() }}>Apply reviewed migration</Button><OperationFeedback operation={operation} /></Section>}</>
}

function StorageSettings({ settings, owner }: { settings: PanelSettings; owner: boolean }) {
  const operation = useOperation<PanelSettings>()
  const [replace, setReplace] = useState(false)
  const confirm = useConfirm()
  return <Section title="R2 / S3 backup storage" description="Only public HTTPS endpoints are accepted. Credentials are encrypted on the host and never returned to this page."><p><Status value={settings.storage.configured ? 'configured' : 'not configured'} /></p><SubmitForm onSubmit={async form => {
    if (!await confirm({ title: 'Save backup storage configuration?', body: <p>New credentials explicitly replace the saved configuration. An empty credential field is never used to erase a saved secret silently.</p>, action: 'Save storage' })) return
    const storage = { enabled: form.get('enabled') === 'on', ...(replace ? Object.fromEntries(['endpoint', 'region', 'bucket', 'access_key', 'secret_key'].map(key => [key, String(form.get(key))])) : {}) }
    await operation.run('/settings', { storage }, 'PUT'); setReplace(false)
  }}><fieldset disabled={!owner || operation.isPending}><CheckField name="enabled" defaultChecked={settings.storage.enabled}>Enable remote backup storage</CheckField><CheckField checked={replace} onChange={event => setReplace(event.target.checked)}>Explicitly replace storage endpoint and credentials</CheckField>{replace && <div className="form-grid"><Field label="HTTPS endpoint"><input type="url" name="endpoint" required pattern="https://.*" placeholder="https://account.r2.cloudflarestorage.com" /></Field><Field label="Region"><input name="region" required placeholder="auto" /></Field><Field label="Bucket"><input name="bucket" required /></Field><Field label="Access key"><input name="access_key" required autoComplete="off" /></Field><Field label="Secret key"><input name="secret_key" type="password" required autoComplete="new-password" /></Field></div>}{!settings.storage.configured && !replace && <p className="muted">Select explicit replacement to configure a new storage destination.</p>}{owner && <Button type="submit" busy={operation.isPending}>Save storage configuration</Button>}</fieldset></SubmitForm><SavedFeedback operation={operation} />{!owner && <p className="read-only">Only the owner can configure credentials.</p>}</Section>
}

function NotificationSettings({ settings, owner }: { settings: PanelSettings; owner: boolean }) {
  const operation = useOperation<PanelSettings>()
  const [replace, setReplace] = useState(false)
  const [kind, setKind] = useState('discord')
  return <Section title="Notifications" description="Choose Discord, a public HTTPS webhook, or SMTP using implicit TLS on port 465. Saved secrets are never displayed."><p><Status value={settings.notifications.configured ? 'configured' : 'not configured'} /></p><SubmitForm onSubmit={async form => {
    const credentials = replace ? kind === 'smtp' ? { kind, smtp_host: String(form.get('smtp_host')), smtp_username: String(form.get('smtp_username')), smtp_password: String(form.get('smtp_password')), from: String(form.get('from')), to: String(form.get('to')).split(',').map(email => email.trim()).filter(Boolean) } : { kind, url: String(form.get('url')) } : {}
    await operation.run('/settings', { notifications: { enabled: form.get('enabled') === 'on', ...credentials } }, 'PUT'); setReplace(false)
  }}><fieldset disabled={!owner || operation.isPending}><CheckField name="enabled" defaultChecked={settings.notifications.enabled}>Enable notifications</CheckField><CheckField checked={replace} onChange={event => setReplace(event.target.checked)}>Explicitly replace notification destination and credentials</CheckField>{replace && <><Field label="Delivery kind"><select value={kind} onChange={event => setKind(event.target.value)}><option value="discord">Discord webhook</option><option value="webhook">HTTPS webhook</option><option value="smtp">SMTP over TLS (port 465)</option></select></Field>{kind === 'smtp' ? <div className="form-grid"><Field label="SMTP hostname" hint="Hostname only; the server uses TLS port 465."><input name="smtp_host" required /></Field><Field label="SMTP username"><input name="smtp_username" required autoComplete="off" /></Field><Field label="SMTP password"><input name="smtp_password" type="password" required autoComplete="new-password" /></Field><Field label="Sender email"><input name="from" type="email" required /></Field><Field label="Recipient emails" hint="Separate multiple recipients with commas."><input name="to" type="email" multiple required /></Field></div> : <Field label={kind === 'discord' ? 'Discord webhook URL' : 'Webhook URL'}><input name="url" type="url" required pattern="https://.*" autoComplete="off" /></Field>}</>}{owner && <Button type="submit" busy={operation.isPending}>Save notifications</Button>}</fieldset></SubmitForm><SavedFeedback operation={operation} />{!owner && <p className="read-only">Only the owner can configure credentials.</p>}</Section>
}

function Panel({ user }: { user: User }) {
  const settings = useAPI<PanelSettings>('/settings')
  const operation = useOperation<PanelSettings>()
  const owner = user.role === 'owner'
  return <QueryState query={settings}>{data => <><Section title="Panel settings" description={`Version ${data.version} · Runtime provider: ${data.runtime_provider}`}><SubmitForm onSubmit={form => operation.run('/settings', { backup_retention: { enabled: form.get('retention_enabled') === 'on', keep_last: Number(form.get('keep_last')) } }, 'PUT').then(() => {})}><fieldset disabled={operation.isPending}><div className="form-grid"><Field label="Public panel URL" hint="Managed by the root-owned host configuration. Change this value on the panel host, not in the browser."><input type="url" value={data.public_url} readOnly /></Field><Field label="Backups to retain" hint="Retention must preserve the configured minimum recovery coverage."><input name="keep_last" type="number" min={1} max={10000} required defaultValue={data.backup_retention.keep_last} /></Field></div><CheckField name="retention_enabled" defaultChecked={data.backup_retention.enabled}>Enable backup retention</CheckField><Button type="submit" variant="primary" busy={operation.isPending}>Save panel settings</Button></fieldset></SubmitForm><SavedFeedback operation={operation} /><dl className="key-values"><div><dt>Adoption roots</dt><dd>{data.adopt_roots.length ? data.adopt_roots.map(path => <code className="block" key={path}>{path}</code>) : 'None configured on host'}</dd></div><div><dt>Java allowlist</dt><dd>{data.java_paths.length ? data.java_paths.map(path => <code className="block" key={path}>{path}</code>) : 'None configured on host'}</dd></div></dl></Section><StorageSettings settings={data} owner={owner} /><NotificationSettings settings={data} owner={owner} /></>}</QueryState>
}

function Users({ instances }: { instances: Instance[] }) {
  const users = useAPI<User[]>('/users')
  const operation = useOperation<User>()
  const [role, setRole] = useState('viewer')
  const confirm = useConfirm()
  return <><Section title="Panel users"><QueryState query={users} empty={<Empty title="No users returned">Check the owner account and refresh.</Empty>}>{data => <DataTable label="Panel users" rows={data} rowKey={user => user.id} columns={[{ header: 'Username', render: user => <strong>{user.username}</strong> }, { header: 'Role', render: user => user.role }, { header: 'Two-factor', render: user => user.two_factor ? 'Enabled' : 'Not enabled' }]} />}</QueryState></Section><Section title="Create a user" description="Operators and viewers require explicit instance grants. No grant means no access."><SubmitForm onSubmit={async (data, form) => {
    const ids = data.getAll('instance_ids').map(String)
    if (role === 'admin' && !await confirm({ title: 'Create an administrator?', body: <p>Administrators can modify all instances, files, content, backups, runtime, and panel update settings.</p>, action: 'Create administrator', danger: true })) return
    await operation.run('/users', { username: data.get('username'), password: data.get('password'), role, instance_ids: ids }); form.reset(); setRole('viewer')
  }}><fieldset disabled={operation.isPending}><div className="form-grid"><Field label="Username"><input name="username" required minLength={3} maxLength={64} autoComplete="off" /></Field><Field label="Initial password"><input name="password" type="password" required minLength={12} maxLength={1024} autoComplete="new-password" /></Field><Field label="Role"><select value={role} onChange={event => setRole(event.target.value)}><option value="viewer">Viewer — read only</option><option value="operator">Operator — lifecycle, console, players, backups</option><option value="admin">Administrator — all instances</option></select></Field></div>{role !== 'admin' && <fieldset className="instance-grants"><legend>Instance grants</legend>{instances.length ? instances.map(instance => <CheckField key={instance.id} name="instance_ids" value={instance.id}>{instance.name}</CheckField>) : <p>No instances exist yet. This user will have no instance access.</p>}</fieldset>}<Button type="submit" variant="primary" busy={operation.isPending}><UserPlus size={16} />Create user</Button></fieldset></SubmitForm>{operation.error && <ErrorNotice error={operation.error} />}{operation.isSuccess && <Notice tone="success">User account created.</Notice>}</Section></>
}

function Account({ user }: { user: User }) {
  const enroll = useOperation<{ secret: string; uri?: string }>()
  const enable = useOperation()
  const client = useQueryClient()
  return <Section title="Account security" description={`Signed in as ${user.username} · ${user.role}`}><p><Status value={user.two_factor ? 'two-factor enabled' : 'two-factor not enabled'} /></p>{!user.two_factor && <><p>Add a TOTP authenticator. Keep its recovery information outside this panel.</p><Button busy={enroll.isPending} onClick={() => { void enroll.run('/auth/totp', {}).catch(() => {}) }}><KeyRound size={16} />Generate authenticator secret</Button>{enroll.error && <ErrorNotice error={enroll.error} />}{enroll.data && <><Notice tone="warning">This secret is shown only for enrollment. Do not share it or include it in a support screenshot.</Notice><Field label="Authenticator secret"><input value={enroll.data.secret} readOnly autoComplete="off" /></Field><SubmitForm onSubmit={async data => { await enable.run('/auth/totp', { password: data.get('password'), secret: enroll.data!.secret, code: data.get('code') }); setSession(); client.clear(); window.location.reload() }}><div className="form-grid"><Field label="Current password"><input name="password" type="password" autoComplete="current-password" required /></Field><Field label="Six-digit authenticator code"><input name="code" inputMode="numeric" autoComplete="one-time-code" pattern="[0-9]{6}" maxLength={6} required /></Field></div><Button type="submit" variant="primary" busy={enable.isPending}><ShieldCheck size={16} />Enable two-factor and sign in again</Button></SubmitForm>{enable.error && <ErrorNotice error={enable.error} />}</>}</>}</Section>
}

function Audit() {
  const audit = useAPI<AuditEntry[]>('/audit')
  return <Section title="Audit history" description="Server-recorded actions, actors, and instance scope."><QueryState query={audit} empty={<Empty title="No audit entries returned">New administrative activity will appear here.</Empty>}>{data => <DataTable label="Audit history" rows={data} rowKey={entry => entry.id} columns={[{ header: 'Time', render: entry => <DateText value={entry.time} /> }, { header: 'Action', render: entry => entry.action }, { header: 'Actor ID', render: entry => <code>{entry.user_id}</code> }, { header: 'Instance', render: entry => <code>{entry.instance_id || 'Panel'}</code> }, { header: 'Detail', render: entry => entry.detail || 'No detail' }]} />}</QueryState></Section>
}

export function Settings({ instance, instances, user }: { instance?: Instance; instances: Instance[]; user: User }) {
  const [tab, setTab] = useState(instance ? 'server' : 'account')
  const tabs = [...(instance ? [{ id: 'server', label: 'Server properties' }, { id: 'runtime', label: 'Fabric & Java' }] : []), ...(canAdmin(user) ? [{ id: 'panel', label: 'Panel' }, { id: 'audit', label: 'Audit' }] : []), ...(user.role === 'owner' ? [{ id: 'users', label: 'Users & access' }] : []), { id: 'account', label: 'Account security' }]
  const active = tabs.some(item => item.id === tab) ? tab : 'account'
  return <><PageHeading title="Settings" description="Instance configuration, panel preferences, and access controls." /><div className="tabs" aria-label="Settings sections">{tabs.map(item => <Button key={item.id} className={active === item.id ? 'active' : ''} variant="ghost" aria-pressed={active === item.id} onClick={() => setTab(item.id)}>{item.label}</Button>)}</div>{active === 'server' && instance && <Properties instance={instance} user={user} />}{active === 'runtime' && instance && <Runtime instance={instance} user={user} />}{active === 'panel' && canAdmin(user) && <Panel user={user} />}{active === 'users' && user.role === 'owner' && <Users instances={instances} />}{active === 'audit' && canAdmin(user) && <Audit />}{active === 'account' && <Account user={user} />}</>
}
