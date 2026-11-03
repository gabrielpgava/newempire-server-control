import { useEffect, useRef, useState } from 'react'
import { Send, RefreshCw } from 'lucide-react'
import { canOperate, instancePath, useAPI, useOperation, type Instance, type JobDetail, type LogLine, type RuntimeStatus, type User } from '../api'
import { Button, CheckField, ErrorNotice, Field, JobResult, Notice, PageHeading, QueryState, ReadOnly, Section, SubmitForm } from '../components'
import { LifecycleActions } from './Dashboard'

export function Console({ instance, user }: { instance: Instance; user: User }) {
  const logs = useAPI<LogLine[]>(instancePath(instance.id, '/logs'), { interval: 5_000 })
  const status = useAPI<RuntimeStatus>(instancePath(instance.id, '/status'), { interval: 5_000 })
  const operation = useOperation()
  const commandJob = useAPI<JobDetail>(`/jobs/${operation.data?.id ?? ''}`, { enabled: !!operation.data?.id, interval: 2_000 })
  const [command, setCommand] = useState('')
  const [filter, setFilter] = useState('')
  const [follow, setFollow] = useState(true)
  const end = useRef<HTMLDivElement>(null)
  const allowed = canOperate(user, instance.id)
  useEffect(() => { if (follow) end.current?.scrollIntoView({ block: 'nearest' }) }, [logs.data, follow])
  async function send() { if (!command.trim()) return; await operation.run(instancePath(instance.id, '/console'), { command: command.trim() }); setCommand('') }
  return <><PageHeading title="Console" description="Live service logs and Minecraft commands. This is not a host shell." actions={<LifecycleActions instance={instance} user={user} status={status.data} />} />
    {status.error && <ErrorNotice error={status.error} />}
    <Section title="Server log" actions={<><CheckField checked={follow} onChange={event => setFollow(event.target.checked)}>Follow latest</CheckField><Button busy={logs.isFetching} onClick={() => { void logs.refetch() }}><RefreshCw size={15} />Refresh</Button></>}>
      <Field label="Filter visible log lines"><input type="search" value={filter} onChange={event => setFilter(event.target.value)} placeholder="Find a message" /></Field>
      <QueryState query={logs} empty={<Notice>No service log lines are available. Start the service or check the runtime configuration.</Notice>}>{data => <div className="console-log" role="region" aria-label="Minecraft server log" tabIndex={0}>{data.filter(line => line.message.toLowerCase().includes(filter.toLowerCase())).map((line, index) => <div className="log-line" key={`${line.time}-${index}`}><time dateTime={line.time}>{new Date(line.time).toLocaleTimeString()}</time><span>{line.message}</span></div>)}{data.every(line => !line.message.toLowerCase().includes(filter.toLowerCase())) && <p>No log lines match this filter.</p>}<div ref={end} /></div>}</QueryState>
    </Section>
    <Section title="Send a command" description="Commands run through local RCON and are recorded in the audit history.">
      {!allowed && <ReadOnly />}
      {!status.data?.ready && <Notice tone="warning">Readiness is not confirmed. Commands require a working local RCON connection.</Notice>}
      <SubmitForm onSubmit={send} className="console-command"><Field label="Minecraft command"><input value={command} onChange={event => setCommand(event.target.value)} name="command" placeholder="list" autoComplete="off" maxLength={2048} required disabled={!allowed} /></Field><Button type="submit" variant="primary" busy={operation.isPending} disabled={!allowed || !command.trim() || !status.data?.ready}><Send size={16} />Send command</Button></SubmitForm>
      {operation.error && <ErrorNotice error={operation.error} />}{commandJob.error && <ErrorNotice error={commandJob.error} />}{commandJob.data?.job.result && typeof commandJob.data.job.result.output === 'string' && <pre className="command-output" role="status">{commandJob.data.job.result.output || 'Command returned no output.'}</pre>}{operation.data?.id && <JobResult job={commandJob.data?.job ?? operation.data} />}
    </Section>
  </>
}
