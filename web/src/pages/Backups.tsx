import { useState } from 'react'
import { Archive, CheckCircle2, Download, RotateCcw } from 'lucide-react'
import { API, canAdmin, canOperate, formatBytes, instancePath, useAPI, useOperation, type Backup, type Instance, type RestorePreview, type User } from '../api'
import { Button, CheckField, DataTable, DateText, Empty, ErrorNotice, Field, Notice, OperationFeedback, PageHeading, QueryState, ReadOnly, Section, Status, SubmitForm, useConfirm } from '../components'
import { ImportReport } from './Files'

export function Backups({ instance, user }: { instance: Instance; user: User }) {
  const backups = useAPI<Backup[]>(instancePath(instance.id, '/backups'))
  const operation = useOperation()
  const [selected, setSelected] = useState<Backup>()
  const preview = useAPI<RestorePreview>(instancePath(instance.id, `/backups/${selected?.id ?? ''}/preview`), { enabled: !!selected && canAdmin(user) })
  const confirm = useConfirm()
  const allowed = canOperate(user, instance.id)
  async function restore() {
    if (!selected || !preview.data) return
    if (await confirm({ title: `Restore ${instance.name}?`, body: <><p>This explicitly replaces world and instance data with the selected backup. Changes made after this backup will no longer be active.</p><p>The server must shut down gracefully. A recovery point must be created before the restore is committed.</p></>, action: 'Restore backup', danger: true, verify: instance.name })) await operation.run(instancePath(instance.id, `/backups/${selected.id}/restore`), { confirm: true }).catch(() => {})
  }
  return <><PageHeading title="Backups" description="Verified restore points stored outside the instance directory." />
    {!allowed && <ReadOnly />}<OperationFeedback operation={operation} />
    {allowed && <Section title="Create a restore point" description="Offline backups are the default. Online backups require coordinated save suspension and resume."><SubmitForm onSubmit={data => operation.run(instancePath(instance.id, '/backups'), { note: String(data.get('note')), online: data.get('online') === 'on' }).then(() => {})}><div className="inline-form"><Field label="Backup note (optional)"><input name="note" maxLength={500} placeholder="Before a content update" /></Field><Button type="submit" variant="primary" busy={operation.isPending}><Archive size={16} />Create backup</Button></div><CheckField name="online">Use a coordinated online backup if the server is running</CheckField></SubmitForm></Section>}
    <Section title="Restore points"><QueryState query={backups} empty={<Empty title="No backups yet">Create a restore point before imports, restores, or content changes.</Empty>}>{data => <DataTable label="Backups" rows={data} rowKey={backup => backup.id} columns={[
      { header: 'Created', render: backup => <><DateText value={backup.created_at} /><small className="table-secondary identifier">{backup.id.slice(0, 12)}</small></> },
      { header: 'Note', render: backup => backup.note || 'No note' },
      { header: 'Size', render: backup => formatBytes(backup.size) },
      { header: 'Integrity', render: backup => <Status value={backup.verified ? 'verified' : 'unverified'} /> },
      { header: 'Actions', render: backup => <div className="row-actions">{allowed && <a className="button button-ghost" href={`${API}${instancePath(instance.id, `/backups/${backup.id}/download`)}`} aria-label={`Download backup ${backup.id.slice(0, 8)}`}><Download size={16} />Download</a>}{allowed && <Button disabled={operation.isPending} onClick={() => { void operation.run(instancePath(instance.id, `/backups/${backup.id}/verify`)).catch(() => {}) }}><CheckCircle2 size={15} />Verify</Button>}{canAdmin(user) && <Button onClick={() => setSelected(backup)}><RotateCcw size={15} />Restore preview</Button>}</div> },
    ]} />}</QueryState></Section>
    {selected && <Section title="Restore preview" description={`Backup ${selected.id}`} actions={<Button onClick={() => setSelected(undefined)}>Close preview</Button>}><Notice tone="warning">World recovery is an explicit administrator operation. This preview does not modify files.</Notice><QueryState query={preview}>{report => <><ImportReport report={report} /><p className="muted">Expected archive SHA-256: <code>{selected.sha256}</code></p><Button variant="danger" busy={operation.isPending} onClick={() => { void restore() }}><RotateCcw size={16} />Restore this backup</Button></>}</QueryState></Section>}
  </>
}
