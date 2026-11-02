import { Play, RotateCw, Square, Terminal, Archive, ArrowRight } from 'lucide-react'
import { canOperate, formatBytes, instancePath, isActiveJob, useAPI, useOperation, type Backup, type Instance, type Job, type RuntimeStatus, type User } from '../api'
import { Button, DataTable, DateText, Empty, Notice, OperationFeedback, PageHeading, QueryState, ReadOnly, Section, Status, Timeline, useConfirm } from '../components'

export function LifecycleActions({ instance, status, user }: { instance: Instance; status?: RuntimeStatus; user: User }) {
  const operation = useOperation()
  const confirm = useConfirm()
  const allowed = canOperate(user, instance.id)
  async function perform(action: 'start' | 'stop' | 'restart') {
    if (action !== 'start' && !await confirm({ title: `${action === 'stop' ? 'Stop' : 'Restart'} ${instance.name}?`, body: <p>Connected players will be disconnected. The server will shut down gracefully; follow the job before making file changes.</p>, action: action === 'stop' ? 'Stop server' : 'Restart server', danger: true })) return
    await operation.run(instancePath(instance.id, `/actions/${action}`)).catch(() => {})
  }
  return <div className="lifecycle-actions"><div className="actions"><Button variant="primary" disabled={!allowed || !instance.eula || ['ready', 'starting', 'stopping'].includes(status?.state ?? '')} busy={operation.isPending} onClick={() => { void perform('start') }}><Play size={16} />Start</Button><Button disabled={!allowed || status?.state === 'offline' || operation.isPending} onClick={() => { void perform('stop') }}><Square size={15} />Stop</Button><Button disabled={!allowed || !instance.eula || operation.isPending} onClick={() => { void perform('restart') }}><RotateCw size={16} />Restart</Button></div><OperationFeedback operation={operation} /></div>
}

export function JobTable({ jobs }: { jobs: Job[] }) {
  return <DataTable label="Recent jobs" rows={jobs} rowKey={job => job.id} columns={[
    { header: 'Operation', render: job => <a className="strong" href={`#/jobs/${job.id}`}>{job.kind.replaceAll('_', ' ')}</a> },
    { header: 'State', render: job => <Status value={job.state} /> },
    { header: 'Progress', render: job => <span className="job-progress-text">{job.step || (isActiveJob(job) ? 'Waiting for an update' : job.error || 'No further detail reported')}{typeof job.progress === 'number' && <progress value={job.progress} max={100} aria-label={`${job.kind} progress`} />}</span> },
    { header: 'Created', render: job => <DateText value={job.created_at} /> },
    { header: 'ID', className: 'identifier', render: job => <a href={`#/jobs/${job.id}`}>{job.id.slice(0, 8)}</a> },
  ]} />
}

export function Dashboard({ instance, user }: { instance: Instance; user: User }) {
  const status = useAPI<RuntimeStatus>(instancePath(instance.id, '/status'), { interval: 5_000 })
  const backups = useAPI<Backup[]>(instancePath(instance.id, '/backups'))
  const jobs = useAPI<Job[]>('/jobs', { interval: 5_000 })
  const verified = backups.data?.some(backup => backup.verified)
  const active = jobs.data?.filter(job => job.instance_id === instance.id && isActiveJob(job)) ?? []
  return <>
    <PageHeading title={instance.name} description={`Fabric · Minecraft ${instance.minecraft_version || 'version not recorded'} · Port ${instance.port}`} actions={<a className="button button-secondary" href={`#/instances/${instance.id}/console`}><Terminal size={16} />Open console</a>} />
    <Timeline stages={[
      { label: 'Registered', detail: 'Instance isolated', complete: true },
      { label: 'EULA accepted', detail: instance.eula ? 'Permission recorded' : 'Acceptance required', complete: instance.eula },
      { label: 'Service running', detail: status.data ? status.data.state : 'Not observed', complete: status.data?.ready === true || (status.data?.pid ?? 0) > 0, current: instance.eula && !status.data?.ready },
      { label: 'Server ready', detail: status.data?.ready ? 'Local RCON responding' : 'Not confirmed', complete: status.data?.ready === true },
    ]} />
    {!instance.eula && <Notice tone="warning">The Minecraft EULA has not been accepted for this instance. The server cannot be started.</Notice>}
    {active.length > 0 && <Notice>{active.length} active {active.length === 1 ? 'job is' : 'jobs are'} using this instance. <a href="#/jobs">Review jobs<ArrowRight size={14} /></a></Notice>}
    <Section title="Current observation" actions={<LifecycleActions instance={instance} user={user} status={status.data} />}>
      <QueryState query={status}>{data => <><div className="observation-strip"><div><span>Service</span><Status value={data.state} /></div><div><span>Readiness</span><strong>{data.ready ? 'Ready for connections' : 'Not confirmed'}</strong></div><div><span>Memory in use</span><strong>{formatBytes(data.memory_bytes)}</strong></div><div><span>Uptime</span><strong>{data.uptime_seconds > 0 ? `${Math.floor(data.uptime_seconds / 3600)}h ${Math.floor(data.uptime_seconds % 3600 / 60)}m` : 'Not running'}</strong></div><div><span>Players</span><strong>{data.players?.length ?? 'Not reported'}</strong></div></div>{data.detail && <Notice tone={data.ready ? 'info' : 'warning'}>{data.detail}</Notice>}</>}</QueryState>
      {!canOperate(user, instance.id) && <ReadOnly />}
    </Section>
    <div className="dashboard-split"><Section title="Recovery coverage" actions={<a href={`#/instances/${instance.id}/backups`}>Manage backups<ArrowRight size={14} /></a>}><QueryState query={backups} empty={<Empty title="No backups recorded" action={<a className="button button-secondary" href={`#/instances/${instance.id}/backups`}><Archive size={16} />Create a backup</a>}>Create and verify a backup before changing this server.</Empty>}>{data => <><p className="lead">{verified ? 'A verified restore point is recorded.' : 'No verified restore point is recorded.'}</p><p className="muted">Latest backup: <DateText value={data[0]?.created_at} />. Verification checks the stored archive; it is not a restore test.</p></>}</QueryState></Section><Section title="Instance record"><dl className="key-values"><div><dt>Instance ID</dt><dd><code>{instance.id}</code></dd></div><div><dt>Fabric loader</dt><dd>{instance.loader_version || 'Not recorded'}</dd></div><div><dt>Memory limit</dt><dd>{instance.memory_mb.toLocaleString()} MiB</dd></div><div><dt>Source</dt><dd>{instance.adopted ? 'Adopted local directory' : 'Managed directory'}</dd></div></dl></Section></div>
    <Section title="Recent activity" actions={<a href="#/jobs">All jobs<ArrowRight size={14} /></a>}><QueryState query={jobs} empty={<Empty title="No jobs yet">Lifecycle and file operations will appear here.</Empty>}>{all => { const filtered = all.filter(job => job.instance_id === instance.id).slice(0, 8); return filtered.length ? <JobTable jobs={filtered} /> : <Empty title="No jobs for this instance">Start a server or create a backup to record an operation.</Empty> }}</QueryState></Section>
  </>
}
