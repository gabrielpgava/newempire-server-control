import { CalendarClock, Trash2 } from 'lucide-react'
import { canAdmin, useAPI, useOperation, type Instance, type Schedule, type User } from '../api'
import { Button, CheckField, DataTable, DateText, Empty, ErrorNotice, Field, Notice, PageHeading, QueryState, ReadOnly, Section, Status, SubmitForm, useConfirm } from '../components'

const days = ['Sun', 'Mon', 'Tue', 'Wed', 'Thu', 'Fri', 'Sat']
export function Automation({ instance, user }: { instance: Instance; user: User }) {
  const schedules = useAPI<Schedule[]>('/schedules')
  const operation = useOperation<Schedule | { ok: boolean }>()
  const confirm = useConfirm()
  const allowed = canAdmin(user)
  async function remove(schedule: Schedule) { if (await confirm({ title: 'Delete this schedule?', body: <p>This stops future scheduled runs. Existing jobs and backups are retained.</p>, action: 'Delete schedule', danger: true })) await operation.run(`/schedules/${schedule.id}`, undefined, 'DELETE').catch(() => {}) }
  return <><PageHeading title="Automation" description="Scheduled work uses the same validated jobs and per-instance locks as manual actions." />{!allowed && <ReadOnly admin />}{operation.error && <ErrorNotice error={operation.error} />}{operation.isSuccess && <Notice tone="success">The schedule change was saved.</Notice>}
    <Section title="Instance schedules"><QueryState query={schedules}>{data => { const filtered = data.filter(schedule => schedule.instance_id === instance.id); return filtered.length ? <DataTable label="Instance schedules" rows={filtered} rowKey={schedule => schedule.id} columns={[
      { header: 'Operation', render: schedule => schedule.kind.replaceAll('_', ' ') },
      { header: 'Time', render: schedule => <>{String(schedule.hour).padStart(2, '0')}:{String(schedule.minute).padStart(2, '0')}<small className="table-secondary">{schedule.timezone}</small></> },
      { header: 'Days', render: schedule => schedule.weekdays.map(day => days[day]).join(', ') },
      { header: 'State', render: schedule => <Status value={schedule.enabled ? 'enabled' : 'disabled'} /> },
      { header: 'Next run', render: schedule => schedule.enabled ? <DateText value={schedule.next_run} /> : 'Disabled' },
      { header: 'Last job', render: schedule => schedule.last_job_id ? <a href={`#/jobs/${schedule.last_job_id}`}>{schedule.last_job_id.slice(0, 8)}</a> : 'Not recorded' },
      { header: 'Actions', render: schedule => allowed && <Button variant="ghost" disabled={operation.isPending} onClick={() => { void remove(schedule) }}><Trash2 size={15} />Delete</Button> },
    ]} /> : <Empty title="No schedules for this instance">Create a backup, restart, or mod-check schedule below.</Empty> }}</QueryState></Section>
    {allowed && <Section title="Create a schedule" description="Times use an explicit IANA timezone. A mod check does not silently enable automatic updates."><SubmitForm onSubmit={async (data, form) => {
      const weekdays = data.getAll('weekdays').map(Number)
      if (!weekdays.length) throw new Error('Choose at least one weekday.')
      const [hour, minute] = String(data.get('time')).split(':').map(Number)
      if (data.get('kind') === 'restart' && !await confirm({ title: 'Schedule automatic restarts?', body: <p>Players will be disconnected during scheduled graceful restarts. Select an appropriate maintenance time.</p>, action: 'Create restart schedule', danger: true })) return
      await operation.run('/schedules', { instance_id: instance.id, kind: data.get('kind'), hour, minute, weekdays, timezone: data.get('timezone'), enabled: data.get('enabled') === 'on' }); form.reset()
    }}><fieldset disabled={operation.isPending}><div className="form-grid"><Field label="Operation"><select name="kind"><option value="backup">Backup</option><option value="restart">Graceful restart</option><option value="mod_check">Mod update check</option></select></Field><Field label="Local time"><input name="time" type="time" required defaultValue="04:00" /></Field><Field label="IANA timezone"><input name="timezone" required defaultValue={Intl.DateTimeFormat().resolvedOptions().timeZone || 'UTC'} /></Field></div><fieldset className="weekday-picker"><legend>Weekdays</legend>{days.map((day, index) => <CheckField name="weekdays" value={index} key={day} defaultChecked>{day}</CheckField>)}</fieldset><CheckField name="enabled" defaultChecked>Enable schedule</CheckField><Button type="submit" variant="primary" busy={operation.isPending}><CalendarClock size={16} />Create schedule</Button></fieldset></SubmitForm></Section>}
  </>
}
