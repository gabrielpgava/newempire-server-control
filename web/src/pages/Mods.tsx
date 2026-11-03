import { useState } from 'react'
import { Download, LockKeyhole, Search, ShieldCheck } from 'lucide-react'
import { API, canAdmin, instancePath, useAPI, useOperation, type Instance, type Mod, type ModPlan, type ModPolicy, type ModSearchResult, type User } from '../api'
import { Button, CheckField, DataTable, Empty, ErrorNotice, Field, Notice, OperationFeedback, PageHeading, PlanView, QueryState, ReadOnly, Section, Status, SubmitForm, useConfirm } from '../components'
import { UploadPanel, type CompletedUpload } from '../uploads'

function ModPlanContents({ plan }: { plan: ModPlan }) {
  return <><PlanView plan={plan} />{plan.actions.length > 0 && <DataTable label="Planned mod changes" rows={plan.actions} rowKey={action => `${action.project_id}-${action.filename}`} columns={[{ header: 'Project', render: action => action.name }, { header: 'From', render: action => action.from_version || 'Not installed' }, { header: 'To', render: action => action.to_version }, { header: 'Reason', render: action => action.reason }]} />}{plan.blocked.length > 0 && <Notice tone="warning"><strong>The plan is blocked</strong><ul>{plan.blocked.map(reason => <li key={reason}>{reason}</li>)}</ul></Notice>}{plan.actions.length === 0 && <Notice>No mod-version changes are listed in this plan. No files have been applied.</Notice>}</>
}

function ServerModpack({ instance }: { instance: Instance }) {
  const [upload, setUpload] = useState<CompletedUpload>()
  const plan = useOperation<ModPlan>()
  const operation = useOperation()
  const confirm = useConfirm()
  function reset() { setUpload(undefined); plan.reset(); operation.reset() }
  async function apply() {
    if (!upload || !plan.data || plan.data.blocked.length) return
    if (await confirm({ title: 'Apply this server modpack?', body: <p>The server will revalidate the staged .mrpack and its reviewed plan, create a restore point, and apply only after its safety checks and graceful shutdown. World recovery is never automatic.</p>, action: 'Apply server modpack', danger: true, verify: instance.name })) await operation.run(instancePath(instance.id, '/modpacks/apply'), { upload_id: upload.id, plan_hash: plan.data.plan_hash, confirm: true }).catch(() => {})
  }
  return <Section title="Server modpack" description="Import a reviewed .mrpack or export managed server content. Export rejects unmanaged or altered JARs." actions={<a className="button button-secondary" href={`${API}${instancePath(instance.id, '/modpacks/export')}`}><Download size={16} />Export .mrpack</a>}>
    <UploadPanel modpack scope={`${instance.id}-modpack`} disabled={plan.isPending || operation.isPending} onReset={reset} onComplete={completed => { setUpload(completed); plan.reset(); operation.reset() }} />
    {upload && <Button busy={plan.isPending} disabled={operation.isPending} onClick={() => { operation.reset(); void plan.run(instancePath(instance.id, '/modpacks/preview'), { upload_id: upload.id }).catch(() => {}) }}>Preview server modpack</Button>}
    {plan.error && <ErrorNotice error={plan.error} />}
    {plan.data && <><h3>Reviewed server modpack plan</h3><ModPlanContents plan={plan.data} /><Button variant="primary" busy={operation.isPending} disabled={!!plan.data.blocked.length || operation.isSuccess} onClick={() => { void apply() }}>Apply reviewed server modpack</Button></>}
    <OperationFeedback operation={operation} />
  </Section>
}

export function Mods({ instance, user }: { instance: Instance; user: User }) {
  const inventory = useAPI<Mod[]>(instancePath(instance.id, '/mods'))
  const policy = useAPI<ModPolicy>(instancePath(instance.id, '/mods/policy'))
  const [query, setQuery] = useState('')
  const [selected, setSelected] = useState<string[]>([])
  const [plannedProjects, setPlannedProjects] = useState<string[]>([])
  const results = useAPI<ModSearchResult[]>(`/mods/search?q=${encodeURIComponent(query)}&minecraft_version=${encodeURIComponent(instance.minecraft_version)}`, { enabled: query.length >= 2 })
  const plan = useOperation<ModPlan>()
  const operation = useOperation()
  const savePolicy = useOperation<ModPolicy>()
  const confirm = useConfirm()
  const allowed = canAdmin(user)
  async function makePlan(projects: string[]) { operation.reset(); setPlannedProjects(projects); await plan.run(instancePath(instance.id, '/mods/plan'), { projects }).catch(() => {}) }
  async function apply() {
    if (!plan.data) return
    if (await confirm({ title: 'Apply the reviewed mod plan?', body: <p>The server will validate dependencies and the unchanged plan, create a restore point, and stop gracefully before applying. Unknown files and version pins are preserved. World data is not rolled back automatically.</p>, action: 'Apply mod plan', danger: true, verify: instance.name })) await operation.run(instancePath(instance.id, '/mods/apply'), { projects: plannedProjects, plan_hash: plan.data.plan_hash, confirm: true }).catch(() => {})
  }
  async function manage(mod: Mod, action: string) {
    if (await confirm({ title: action === 'adopt' ? `Adopt the exact hash of ${mod.name || mod.filename}?` : `${action.charAt(0).toUpperCase() + action.slice(1)} ${mod.name || mod.filename}?`, body: action === 'adopt' ? <p>The server must already be stopped and have a verified backup. Adoption validates the exact file hash, source metadata, and compatibility before recording management metadata. It does not change the JAR bytes.</p> : <p>This is a managed content change. The server validates its safety and records a durable job. Unknown external files are not changed.</p>, action: action === 'adopt' ? 'Confirm adoption' : `Confirm ${action}`, danger: action === 'remove' || action === 'disable' })) { await operation.run(instancePath(instance.id, '/mods/manage'), { operation: action, project_id: mod.project_id, confirm: true }).catch(() => {}); plan.reset() }
  }
  return <><PageHeading title="Fabric mods" description="Hash-based inventory, compatible plans, and explicit version pins. Fabric mods are not Paper plugins." actions={allowed && <Button variant="primary" busy={plan.isPending} onClick={() => { void makePlan([]) }}><ShieldCheck size={16} />Plan managed updates</Button>} />
    {!allowed && <ReadOnly admin />}<OperationFeedback operation={operation} />
    <Section title="Installed content"><QueryState query={inventory} empty={<Empty title="No mods detected">Search Modrinth below to plan your first installation.</Empty>}>{mods => <DataTable label="Installed Fabric mods" rows={mods} rowKey={mod => mod.filename} columns={[
      { header: 'Mod / file', render: mod => <><strong>{mod.name || mod.filename}</strong><code className="table-secondary">{mod.filename}</code>{mod.reason && <small className="table-secondary">{mod.reason}</small>}</> },
      { header: 'Version', render: mod => mod.version || 'Unidentified' },
      { header: 'Management', render: mod => mod.managed ? <span>{mod.pinned && <LockKeyhole size={14} />} {mod.pinned ? 'Pinned' : 'Managed'}</span> : <Status value="external" /> },
      { header: 'Update', render: mod => mod.update_available ? 'Compatible update reported' : 'No update reported' },
      { header: 'Actions', render: mod => allowed && mod.managed && mod.project_id ? <div className="row-actions"><Button disabled={operation.isPending} onClick={() => { void manage(mod, mod.pinned ? 'unpin' : 'pin') }}>{mod.pinned ? 'Unpin' : 'Pin'}</Button><Button disabled={operation.isPending} onClick={() => { void manage(mod, 'disable') }}>Disable</Button><Button variant="ghost" disabled={operation.isPending} onClick={() => { void manage(mod, 'remove') }}>Remove</Button></div> : allowed && !mod.managed && mod.project_id && mod.version_id ? <Button disabled={operation.isPending} onClick={() => { void manage(mod, 'adopt') }}>Adopt exact hash</Button> : <span className="muted">{mod.managed ? 'Read only' : 'Preserved as external content'}</span> },
    ]} />}</QueryState></Section>
    {allowed && <Section title="Browse Modrinth" description="Stable, server-compatible results. Dependencies and conflicts are checked in the plan."><SubmitForm onSubmit={data => { setQuery(String(data.get('q')).trim()); setSelected([]) }}><div className="inline-form"><Field label="Search Fabric mods"><input name="q" type="search" minLength={2} required placeholder="Search by name" /></Field><Button type="submit"><Search size={16} />Search</Button></div></SubmitForm>{query && <QueryState query={results} empty={<Empty title="No compatible results">Try another search term. No content has been installed.</Empty>}>{data => <><DataTable label="Modrinth search results" rows={data} rowKey={result => result.project_id} columns={[
      { header: 'Select', render: result => <input type="checkbox" aria-label={`Select ${result.title}`} checked={selected.includes(result.project_id)} disabled={result.server_side === 'unsupported'} onChange={event => { setSelected(current => event.target.checked ? [...current, result.project_id] : current.filter(id => id !== result.project_id)); plan.reset() }} /> },
      { header: 'Project', render: result => <><strong>{result.title}</strong><small className="table-secondary">{result.description}</small></> },
      { header: 'ID', render: result => <code>{result.project_id}</code> },
    ]} /><Button variant="primary" busy={plan.isPending} disabled={!selected.length} onClick={() => { void makePlan(selected) }}><Download size={16} />Plan {selected.length} selected {selected.length === 1 ? 'mod' : 'mods'}</Button></>}</QueryState>}</Section>}
    {plan.error && <ErrorNotice error={plan.error} />}{plan.data && <Section title="Reviewed content plan"><ModPlanContents plan={plan.data} /><Button variant="primary" disabled={!allowed || !!plan.data.blocked.length || !plan.data.actions.length || operation.isSuccess} busy={operation.isPending} onClick={() => { void apply() }}>Apply reviewed plan</Button></Section>}
    {allowed && <ServerModpack instance={instance} />}
    <Section title="Update policy" description="Automatic changes are opt-in and limited to compatible managed content."><QueryState query={policy}>{data => <SubmitForm onSubmit={async form => {
      const body = { mode: String(form.get('mode')), interval_hours: Number(form.get('interval_hours')), window_start: String(form.get('window_start')), window_end: String(form.get('window_end')), timezone: String(form.get('timezone')), postpone_players: form.get('postpone_players') === 'on' }
      if (body.mode === 'automatic-safe' && !await confirm({ title: 'Enable unattended compatible updates?', body: <p>Managed, unpinned mods may be updated during the maintenance window after safety checks and a backup. Unknown content is preserved. Choose whether to postpone while players are online.</p>, action: 'Enable automatic-safe updates', danger: true })) return
      await savePolicy.run(instancePath(instance.id, '/mods/policy'), body, 'PUT')
    }}><fieldset disabled={!allowed || savePolicy.isPending}><div className="form-grid"><Field label="Policy"><select name="mode" defaultValue={data.mode}><option value="manual">Manual only</option><option value="notify">Notify only</option><option value="automatic-safe">Automatic compatible updates</option></select></Field><Field label="Check interval (hours)"><input name="interval_hours" type="number" min={1} max={168} defaultValue={data.interval_hours} required /></Field><Field label="Window starts"><input name="window_start" type="time" defaultValue={data.window_start} required /></Field><Field label="Window ends"><input name="window_end" type="time" defaultValue={data.window_end} required /></Field><Field label="IANA timezone"><input name="timezone" defaultValue={data.timezone} required placeholder="UTC" /></Field></div><CheckField name="postpone_players" defaultChecked={data.postpone_players}>Postpone while players are online</CheckField>{allowed && <Button type="submit" busy={savePolicy.isPending}>Save update policy</Button>}</fieldset></SubmitForm>}</QueryState>{savePolicy.error && <ErrorNotice error={savePolicy.error} />}{savePolicy.isSuccess && <Notice tone="success">Update policy saved.</Notice>}</Section>
  </>
}
