import { useEffect, useRef, useState } from 'react'
import { ArrowUp, Download, FileText, Folder, FolderPlus, Pencil, RefreshCw, Trash2, Upload } from 'lucide-react'
import { API, canAdmin, downloadURL, formatBytes, instancePath, request, useAPI, useOperation, validRelativePath, type FileEntry, type ImportPreview, type Instance, type RestorePreview, type User } from '../api'
import { Button, DataTable, DateText, Empty, ErrorNotice, Field, JsonDetails, Notice, OperationFeedback, PageHeading, QueryState, ReadOnly, Section, SubmitForm, useConfirm } from '../components'
import { UploadPanel, type CompletedUpload } from '../uploads'

export function ImportReport({ report }: { report: ImportPreview | RestorePreview }) {
  return <><p className="lead">{report.file_count.toLocaleString()} files · {formatBytes(report.total_bytes)}</p>{report.warnings.length > 0 && <Notice tone="warning"><ul>{report.warnings.map((warning, index) => <li key={index}>{warning}</li>)}</ul></Notice>}{'conflicts' in report && report.conflicts && report.conflicts.length > 0 && <Notice tone="warning"><strong>Existing paths will be affected</strong><ul>{report.conflicts.slice(0, 30).map(path => <li key={path}><code>{path}</code></li>)}</ul></Notice>}<DataTable label="Inspected archive files" rows={report.files.slice(0, 100)} rowKey={entry => entry.path} columns={[{ header: 'Relative path', render: entry => <code>{entry.path}</code> }, { header: 'Size', render: entry => formatBytes(entry.size) }]} />{report.files.length > 100 && <p className="muted">Showing the first 100 files of {report.files.length.toLocaleString()}. The server inspects the complete archive.</p>}{'preview_hash' in report && <JsonDetails value={{ preview_hash: report.preview_hash, launchers: report.launchers, worlds: report.worlds }} label="Inspection identity and detected content" />}</>
}

export function Files({ instance, user }: { instance: Instance; user: User }) {
  const [path, setPath] = useState('.')
  const [filter, setFilter] = useState('')
  const [selected, setSelected] = useState<FileEntry>()
  const [editor, setEditor] = useState<{ path: string; content: string; etag: string }>()
  const [content, setContent] = useState('')
  const [previewBusy, setPreviewBusy] = useState(false)
  const [previewError, setPreviewError] = useState<unknown>(null)
  const [upload, setUpload] = useState<CompletedUpload>()
  const [showRecovery, setShowRecovery] = useState(false)
  const [queuedEdit, setQueuedEdit] = useState(false)
  const [queuedImport, setQueuedImport] = useState(false)
  const previewRequest = useRef<AbortController | null>(null)
  useEffect(() => () => previewRequest.current?.abort(), [])
  const listing = useAPI<FileEntry[]>(instancePath(instance.id, `/files?path=${encodeURIComponent(path)}`))
  const operation = useOperation()
  const importPreview = useOperation<ImportPreview>()
  const allowed = canAdmin(user)
  const recovery = useAPI<{ token: string; path: string; created_at: number }[]>(instancePath(instance.id, '/recovery'), { enabled: allowed && showRecovery })
  const confirm = useConfirm()
  const dirty = editor && editor.content !== content
  async function leaveEditor() { return !dirty || queuedEdit || await confirm({ title: 'Discard unsaved text?', body: <p>Your local draft has not been applied to the server.</p>, action: 'Discard draft', danger: true }) }
  async function openDirectory(destination: string) { if (await leaveEditor()) { previewRequest.current?.abort(); setPreviewBusy(false); setPath(destination); setSelected(undefined); setEditor(undefined); setFilter('') } }
  async function inspect(entry: FileEntry) {
    if (!await leaveEditor()) return
    previewRequest.current?.abort()
    const controller = new AbortController()
    previewRequest.current = controller
    setSelected(entry); setEditor(undefined); setQueuedEdit(false); setPreviewError(null); setPreviewBusy(false)
    if (entry.is_dir) return
    setPreviewBusy(true)
    try {
      const text = await request<{ path: string; content: string; etag: string }>(instancePath(instance.id, `/file?path=${encodeURIComponent(entry.path)}&preview=true`), { signal: controller.signal })
      if (!controller.signal.aborted) { setEditor(text); setContent(text.content) }
    } catch (cause) { if (!controller.signal.aborted) setPreviewError(cause) } finally { if (!controller.signal.aborted) setPreviewBusy(false) }
  }
  async function mutate(body: Record<string, unknown>, message: string, danger = false) {
    if (await confirm({ title: message, body: <p>The server must pass preflight and safety checks. Follow the accepted job; do not assume completion while it is queued.</p>, action: danger ? 'Confirm change' : 'Apply change', danger, ...(danger && selected ? { verify: selected.name } : {}) })) {
      try { await operation.run(instancePath(instance.id, '/files'), { ...body, confirm: true }); if (body.operation === 'write') setQueuedEdit(true) } catch { /* The operation feedback preserves the server error and local draft. */ }
    }
  }
  async function applyImport() {
    if (!upload || !importPreview.data) return
    if (await confirm({ title: 'Apply this import?', body: <p>This changes instance files. The server will create a restore point and apply only after a graceful shutdown and a matching preview. World recovery remains a separate operation.</p>, action: 'Apply import', danger: true, verify: instance.name })) {
      try { await operation.run(instancePath(instance.id, '/imports/apply'), { upload_id: upload.id, preview_hash: importPreview.data.preview_hash, confirm: true }); setQueuedImport(true) } catch { /* Failed application leaves the reviewed upload available for inspection. */ }
    }
  }
  return <><PageHeading title="Files" description="Browse instance files. Changes are staged as jobs; backups live outside this tree." actions={<>{allowed && <Button aria-expanded={showRecovery} onClick={() => setShowRecovery(value => !value)}>Recovery</Button>}<a className="button button-secondary" href={`${API}${instancePath(instance.id, `/export?path=${encodeURIComponent(path)}`)}`}><Download size={16} />Export folder</a><Button onClick={() => { void listing.refetch() }} busy={listing.isFetching}><RefreshCw size={16} />Refresh</Button></>} />
    {!allowed && <ReadOnly admin />}
    <OperationFeedback operation={operation} />
    {showRecovery && allowed && <Section title="File recovery" description="Recover retained paths without overwriting an existing destination."><QueryState query={recovery} empty={<Empty title="No retained paths">Files moved to recovery will appear here.</Empty>}>{entries => <DataTable label="Recoverable paths" rows={entries} rowKey={entry => entry.token} columns={[{ header: 'Original path', render: entry => <code>{entry.path}</code> }, { header: 'Retained', render: entry => <DateText value={entry.created_at} /> }, { header: 'Actions', render: entry => <Button disabled={operation.isPending} onClick={() => { void mutate({ operation: 'recover', token: entry.token }, `Recover ${entry.path}?`) }}>Recover path</Button> }]} />}</QueryState></Section>}
    <Section title="Instance directory"><div className="file-toolbar"><Button aria-label="Open parent directory" disabled={path === '.'} onClick={() => { void openDirectory(path.includes('/') ? path.slice(0, path.lastIndexOf('/')) : '.') }}><ArrowUp size={16} />Up</Button><code className="file-path">{path === '.' ? '/' : `/${path}`}</code><Field label="Filter this directory"><input type="search" value={filter} onChange={event => setFilter(event.target.value)} /></Field></div>
      <QueryState query={listing} empty={<Empty title="This directory is empty">Create a folder or stage an import to add files.</Empty>}>{entries => { const filtered = entries.filter(entry => entry.name.toLowerCase().includes(filter.toLowerCase())).sort((a, b) => Number(b.is_dir) - Number(a.is_dir) || a.name.localeCompare(b.name)); return filtered.length ? <DataTable label="Instance files" rows={filtered} rowKey={entry => entry.path} columns={[
        { header: 'Name', render: entry => <button className="text-button file-name" onClick={() => { void (entry.is_dir ? openDirectory(entry.path) : inspect(entry)) }}>{entry.is_dir ? <Folder size={17} /> : <FileText size={17} />}{entry.name}</button> },
        { header: 'Size', render: entry => entry.is_dir ? 'Directory' : formatBytes(entry.size) },
        { header: 'Modified', render: entry => <DateText value={entry.modified_at} /> },
        { header: 'Actions', render: entry => <div className="row-actions">{!entry.is_dir && <a href={downloadURL(instance.id, entry.path)} download aria-label={`Download ${entry.name}`}><Download size={16} /></a>}{allowed && <Button variant="ghost" aria-label={`Manage ${entry.name}`} onClick={() => { void inspect(entry) }}><Pencil size={16} /></Button>}</div> },
      ]} /> : <Empty title="No names match your filter">Change the filter to see the directory contents.</Empty> }}</QueryState>
      {allowed && <details className="inline-task"><summary><FolderPlus size={17} />Create a folder</summary><SubmitForm onSubmit={data => { const name = String(data.get('name')); if (!validRelativePath(name)) return; return mutate({ operation: 'mkdir', path: path === '.' ? name : `${path}/${name}` }, 'Create this directory?') }}><div className="inline-form"><Field label="Relative folder name"><input name="name" required pattern="[^\\/]+" placeholder="config" /></Field><Button type="submit" busy={operation.isPending}>Create folder</Button></div></SubmitForm></details>}
    </Section>
    {selected && <Section title={selected.path} description={selected.is_dir ? 'Manage this directory.' : 'Bounded UTF-8 preview. Secret and binary files cannot be edited here.'} actions={<Button onClick={() => { void leaveEditor().then(allowed => { if (allowed) { setSelected(undefined); setEditor(undefined) } }) }}>Close file</Button>}>
      {previewBusy && <p role="status">Loading file preview…</p>}{!!previewError && <ErrorNotice error={previewError} />}
      {editor && <><Field label="File content"><textarea className="file-editor" rows={18} spellCheck={false} value={content} readOnly={!allowed || queuedEdit} onChange={event => setContent(event.target.value)} /></Field><div className="actions">{allowed && <Button variant="primary" disabled={!dirty || queuedEdit} busy={operation.isPending} onClick={() => { void mutate({ operation: 'write', path: editor.path, content, etag: editor.etag }, 'Queue this text edit?') }}>Queue text edit</Button>}<Button disabled={previewBusy} onClick={() => { void inspect(selected) }}>Reload latest</Button>{dirty && <span className="muted">{queuedEdit ? 'Edit queued; follow the job before reloading.' : 'Unsaved local draft'}</span>}</div></>}
      {allowed && <div className="file-management"><SubmitForm onSubmit={data => mutate({ operation: 'move', path: selected.path, destination: String(data.get('destination')) }, 'Move this path?', true)}><div className="inline-form"><Field label="Move to relative path"><input name="destination" required defaultValue={selected.path} /></Field><Button type="submit" disabled={operation.isPending}>Move</Button></div></SubmitForm><Button variant="danger" disabled={operation.isPending} onClick={() => { void mutate({ operation: 'trash', path: selected.path }, 'Move this path to recovery?', true) }}><Trash2 size={16} />Move to recovery</Button></div>}
    </Section>}
    {allowed && <Section title="Import an archive or folder" description="Existing paths require a reviewed conflict report. Uploads never write directly into the instance."><UploadPanel key={instance.id} scope={instance.id} disabled={operation.isPending} onReset={() => { setUpload(undefined); importPreview.reset(); setQueuedImport(false) }} onComplete={completed => { setUpload(completed); importPreview.reset(); setQueuedImport(false) }} />{upload && <Button busy={importPreview.isPending} onClick={() => { setQueuedImport(false); void importPreview.run(instancePath(instance.id, '/imports/preview'), { upload_id: upload.id }).catch(() => {}) }}><Upload size={16} />Inspect staged import</Button>}{importPreview.error && <ErrorNotice error={importPreview.error} />}{importPreview.data && <><ImportReport report={importPreview.data} /><Button variant="primary" busy={operation.isPending} disabled={queuedImport} onClick={() => { void applyImport() }}>Apply reviewed import</Button></>}</Section>}
  </>
}
