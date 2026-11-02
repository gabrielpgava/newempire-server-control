import { useState } from 'react'
import { Archive, CheckCircle2, Download, RefreshCw, RotateCcw } from 'lucide-react'
import { API, canAdmin, canOperate, formatBytes, instancePath, useAPI, useOperation, type Backup, type Instance, type RestorePreview, type User } from '../api'
import { Button, CheckField, DataTable, DateText, Empty, Field, Notice, OperationFeedback, PageHeading, QueryState, ReadOnly, Section, Status, SubmitForm, useConfirm } from '../components'
import { ImportReport } from './Files'

export function Backups({ instance, user }: { instance: Instance; user: User }) {
  const backups = useAPI<Backup[]>(instancePath(instance.id, '/backups'))
  const operation = useOperation()
  const [selectedID, setSelectedID] = useState<string>()
  const selected = backups.data?.find(backup => backup.id === selectedID)
  const admin = canAdmin(user)
  const preview = useAPI<RestorePreview>(instancePath(instance.id, `/backups/${selectedID ?? ''}/preview`), { enabled: !!selected && selected.local !== false && admin })
  const confirm = useConfirm()
  const allowed = canOperate(user, instance.id)
  async function restore() {
    if (!selected || selected.local === false || !preview.data || !admin) return
    if (await confirm({ title: `Restore ${instance.name}?`, body: <><p>This explicitly replaces world and instance data with the selected backup. Changes made after this backup will no longer be active.</p><p>The server must shut down gracefully. A recovery point must be created before the restore is committed.</p></>, action: 'Restore backup', danger: true, verify: instance.name })) await operation.run(instancePath(instance.id, `/backups/${selected.id}/restore`), { confirm: true }).catch(() => {})
  }
  return <>
    <PageHeading title="Backups" description="Local and offsite restore points stored outside the instance directory." actions={<Button busy={backups.isFetching} onClick={() => { void backups.refetch() }}><RefreshCw size={16} />Refresh backups</Button>} />
    {!allowed && <ReadOnly />}<OperationFeedback operation={operation} />
    {allowed && <Section title="Create a restore point" description="Offline backups are the default. Online backups require coordinated save suspension and resume.">
      <SubmitForm onSubmit={data => operation.run(instancePath(instance.id, '/backups'), { note: String(data.get('note')), online: data.get('online') === 'on' }).then(() => {})}>
        <div className="inline-form"><Field label="Backup note (optional)"><input name="note" maxLength={500} placeholder="Before a content update" /></Field><Button type="submit" variant="primary" busy={operation.isPending}><Archive size={16} />Create backup</Button></div>
        <CheckField name="online">Use a coordinated online backup if the server is running</CheckField>
      </SubmitForm>
    </Section>}
    <Section title="Restore points">
      <p className="muted">Offsite state and verification labels are stored metadata, not an independent check of remote bytes. Fetching downloads and verifies an offsite copy before local use. Backup downloads are administrator-only because archives can contain RCON credentials.</p>
      <QueryState query={backups} empty={<Empty title="No backups yet">Create a restore point before imports, restores, or content changes.</Empty>}>{data => <DataTable label="Backups" rows={data} rowKey={backup => backup.id} columns={[
        { header: 'Created', render: backup => <><DateText value={backup.created_at} /><small className="table-secondary identifier">{backup.id.slice(0, 12)}</small></> },
        { header: 'Note', render: backup => backup.note || 'No note' },
        { header: 'Size', render: backup => formatBytes(backup.size) },
        { header: 'Creation integrity', render: backup => <Status value={backup.verified ? 'verified' : 'unverified'} label={backup.verified ? 'Verified when created' : undefined} /> },
        { header: 'Local copy', render: backup => <Status value={backup.local === false ? 'not stored locally' : 'available locally'} /> },
        { header: 'Offsite metadata', render: backup => backup.offsite ? <>{backup.offsite.state.replaceAll('_', ' ')}<small className="table-secondary">{backup.offsite.object?.verification ? `Recorded verification: ${backup.offsite.object.verification}` : 'No verification metadata reported'}</small></> : 'Not recorded' },
        { header: 'Actions', render: backup => <div className="row-actions">
          {admin && backup.local !== false && <a className="button button-ghost" href={`${API}${instancePath(instance.id, `/backups/${backup.id}/download`)}`} aria-label={`Download backup ${backup.id.slice(0, 8)}`}><Download size={16} />Download</a>}
          {allowed && <Button disabled={operation.isPending || backup.local === false} onClick={() => { void operation.run(instancePath(instance.id, `/backups/${backup.id}/verify`)).catch(() => {}) }}><CheckCircle2 size={15} />Verify</Button>}
          {admin && <Button disabled={operation.isPending || backup.local === false} onClick={() => setSelectedID(backup.id)}><RotateCcw size={15} />Restore preview</Button>}
          {admin && backup.local === false && backup.offsite && <Button busy={operation.isPending} onClick={() => { void operation.run(instancePath(instance.id, `/backups/${backup.id}/fetch-offsite`), {}).catch(() => {}) }}>Fetch verified offsite copy</Button>}
        </div> },
      ]} />}</QueryState>
    </Section>
    {selected && <Section title="Restore preview" description={`Backup ${selected.id}`} actions={<Button onClick={() => setSelectedID(undefined)}>Close preview</Button>}>
      <Notice tone="warning">World recovery is an explicit administrator operation. This preview does not modify files.</Notice>
      {selected.local === false ? <Notice>Fetch and verify the offsite copy before previewing or restoring this backup.</Notice> : <QueryState query={preview}>{report => <><ImportReport report={report} /><p className="muted">Expected archive SHA-256: <code>{selected.sha256}</code></p><Button variant="danger" busy={operation.isPending} onClick={() => { void restore() }}><RotateCcw size={16} />Restore this backup</Button></>}</QueryState>}
    </Section>}
  </>
}
