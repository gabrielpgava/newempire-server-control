import { useState } from 'react'
import { RefreshCw, X } from 'lucide-react'
import { canOperate, isActiveJob, useAPI, useOperation, type Instance, type Job, type JobDetail, type User } from '../api'
import { Button, DateText, Empty, ErrorNotice, Field, JsonDetails, Notice, PageHeading, QueryState, Section, Status, useConfirm } from '../components'
import { JobTable } from './Dashboard'

export function Jobs({ id, user, instances }: { id?: string; user: User; instances: Instance[] }) {
  const jobs = useAPI<Job[]>('/jobs', { enabled: !id, interval: 4_000 })
  const job = useAPI<JobDetail>(`/jobs/${encodeURIComponent(id ?? '')}`, { enabled: !!id, interval: 3_000 })
  const operation = useOperation()
  const confirm = useConfirm()
  const [instance, setInstance] = useState('')
  const [state, setState] = useState('')
  async function cancel(item: Job) {
    if (await confirm({ title: 'Request job cancellation?', body: <p>Cancellation is cooperative. The server will only stop at a safe point; an applying transaction may need to finish.</p>, action: 'Request cancellation' })) await operation.run(`/jobs/${item.id}/cancel`).catch(() => {})
  }
  if (id) return <><PageHeading title="Job details" description={`Job ${id}`} actions={<a className="button button-secondary" href="#/jobs">Back to jobs</a>} />{operation.error && <ErrorNotice error={operation.error} />}<QueryState query={job}>{({ job: item, steps }) => <><Section title={item.kind.replaceAll('_', ' ')} actions={<><Status value={item.state} />{isActiveJob(item) && item.cancellable === true && canOperate(user, item.instance_id) && <Button busy={operation.isPending} disabled={item.cancel_requested} onClick={() => { void cancel(item) }}><X size={16} />{item.cancel_requested ? 'Cancellation requested' : 'Cancel job'}</Button>}</>}>
    <dl className="key-values"><div><dt>Created</dt><dd><DateText value={item.created_at} /></dd></div><div><dt>Last update</dt><dd><DateText value={item.updated_at} /></dd></div><div><dt>Instance</dt><dd>{instances.find(instance => instance.id === item.instance_id)?.name || item.instance_id || 'Panel operation'}</dd></div></dl>
    {item.step && <p className="lead" role="status">{item.step}</p>}{typeof item.progress === 'number' && <progress value={item.progress} max={100} aria-label="Job progress" />}{item.error && <Notice tone="warning"><strong>The operation did not complete.</strong><p>{item.error}</p><p>Review the diagnostics before retrying. Do not restore world data just because a content update failed.</p></Notice>}
    {item.plan != null && <JsonDetails value={item.plan} label="Transaction plan" />}{item.result != null && <JsonDetails value={item.result} label="Operation result and recovery details" />}
    {steps.length > 0 && <ol className="job-steps">{steps.map((step, index) => <li key={index}><DateText value={step.time} /><span>{step.message}</span></li>)}</ol>}
    {item.result?.instance && <a className="button button-primary" href={`#/instances/${item.result.instance.id}/dashboard`}>Open {item.result.instance.name}</a>}
    {isActiveJob(item) && <p className="muted">Status refreshes automatically. Closing this page does not cancel the job.</p>}
  </Section></>}</QueryState></>
  return <><PageHeading title="Jobs" description="Durable operations, truthful progress, and recovery diagnostics." actions={<Button onClick={() => { void jobs.refetch() }} busy={jobs.isFetching}><RefreshCw size={16} />Refresh</Button>} /><div className="filter-bar"><Field label="Instance"><select value={instance} onChange={event => setInstance(event.target.value)}><option value="">All instances</option>{instances.map(item => <option key={item.id} value={item.id}>{item.name}</option>)}</select></Field><Field label="State"><select value={state} onChange={event => setState(event.target.value)}><option value="">All states</option>{['queued', 'running', 'succeeded', 'failed', 'cancelled', 'interrupted'].map(value => <option key={value}>{value}</option>)}</select></Field></div><Section title="Operation history"><QueryState query={jobs} empty={<Empty title="No jobs recorded">Start an instance, create a backup, or make a managed change to create a job.</Empty>}>{data => { const filtered = data.filter(item => (!instance || item.instance_id === instance) && (!state || item.state === state)); return filtered.length ? <JobTable jobs={filtered} /> : <Empty title="No jobs match these filters">Choose another instance or state.</Empty> }}</QueryState></Section></>
}
