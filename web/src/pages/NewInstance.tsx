import { useState } from 'react'
import { FolderInput, Plus, Server, Upload } from 'lucide-react'
import { useAPI, useOperation, type CreateInstance, type FabricVersions, type ImportPreview, type Instance, type JavaRuntime, type User, canAdmin } from '../api'
import { Button, CheckField, ErrorNotice, Field, JobResult, Notice, PageHeading, QueryState, ReadOnly, Section, SubmitForm, useConfirm } from '../components'
import { UploadPanel, type CompletedUpload } from '../uploads'
import { ImportReport } from './Files'

export function RuntimeFields({ initial, versions, java, onGameChange, installer = false }: { initial?: Instance; versions: FabricVersions; java: JavaRuntime[]; onGameChange?: (version: string) => void; installer?: boolean }) {
  const games = [...new Set([...(initial?.minecraft_version ? [initial.minecraft_version] : []), ...versions.game_versions])]
  const loaders = [...new Set([...(initial?.loader_version ? [initial.loader_version] : []), ...versions.loader_versions])]
  return <div className="form-grid">
    <Field label="Minecraft version"><select name="minecraft_version" defaultValue={initial?.minecraft_version ?? ''} onChange={event => onGameChange?.(event.target.value)} required><option value="" disabled>Choose a stable release</option>{games.map(version => <option key={version}>{version}</option>)}</select></Field>
    <Field label="Fabric loader"><select name="loader_version" defaultValue={initial?.loader_version ?? ''} required><option value="" disabled>Choose a stable loader</option>{loaders.map(version => <option key={version}>{version}</option>)}</select></Field>
    {installer && <Field label="Fabric installer"><select name="installer_version" defaultValue={versions.installer_versions[0] ?? ''} required><option value="" disabled>Choose a stable installer</option>{versions.installer_versions.map(version => <option key={version}>{version}</option>)}</select></Field>}
    <Field label="Java runtime" hint="The server checks version compatibility before making changes."><select name="java_path" defaultValue={initial?.java_path ?? ''} required><option value="" disabled>Choose an installed runtime</option>{java.filter(runtime => !runtime.error).map(runtime => <option key={runtime.path} value={runtime.path}>{runtime.version} · {runtime.path}</option>)}</select></Field>
  </div>
}

export function NewInstance({ user }: { user: User }) {
  const [mode, setMode] = useState<CreateInstance['mode']>('create')
  const [upload, setUpload] = useState<CompletedUpload>()
  const [path, setPath] = useState('')
  const [game, setGame] = useState('')
  const operation = useOperation()
  const preview = useOperation<ImportPreview>()
  const versions = useAPI<FabricVersions>(`/fabric/versions${game ? `?minecraft_version=${encodeURIComponent(game)}` : ''}`, { keepPreviousData: true })
  const java = useAPI<JavaRuntime[]>('/java')
  const roots = useAPI<{ roots: string[] }>('/adoptions', { enabled: canAdmin(user) && mode === 'adopt' })
  const allowed = canAdmin(user)
  const confirm = useConfirm()
  async function create(data: FormData) {
    if (!await confirm({ title: `${mode === 'adopt' ? 'Adopt' : mode === 'import' ? 'Import' : 'Create'} this server?`, body: <p>{mode === 'adopt' ? 'The existing server must be stopped. The panel will validate the unchanged preview and take ownership of service management without silently rewriting its files.' : mode === 'import' ? 'The staged archive will be validated against the reviewed preview and installed in a new isolated instance.' : 'The selected Fabric runtime will be downloaded and verified in a new managed instance. The server will remain stopped.'}</p>, action: mode === 'adopt' ? 'Validate and adopt' : mode === 'import' ? 'Validate and import' : 'Create instance' })) return
    const body: CreateInstance = {
      name: String(data.get('name')), mode, minecraft_version: String(data.get('minecraft_version') ?? ''),
      loader_version: String(data.get('loader_version') ?? ''), java_path: String(data.get('java_path') ?? ''),
      memory_mb: Number(data.get('memory_mb')), port: Number(data.get('port')), rcon_port: Number(data.get('rcon_port')),
      eula: data.get('eula') === 'on', ...(mode === 'adopt' ? { path } : {}),
      ...(mode === 'import' && upload ? { upload_id: upload.id } : {}),
      ...(mode !== 'create' ? { preview_hash: preview.data?.preview_hash, jar: String(data.get('jar')) } : { installer_version: String(data.get('installer_version') ?? '') }),
    }
    await operation.run('/instances', body)
  }
  function choose(value: CreateInstance['mode']) { setMode(value); preview.reset(); operation.reset() }
  return <>
    <PageHeading title="Add an instance" description="Choose where your server begins. Every instance gets its own configuration and service." />
    {!allowed && <ReadOnly admin />}
    <fieldset className="entry-choices" disabled={!allowed || operation.isPending}><legend className="sr-only">Instance source</legend>{[
      { value: 'create', title: 'Create a Fabric server', description: 'A fresh server with versions you choose.', icon: Plus },
      { value: 'import', title: 'Import from your browser', description: 'Upload a server archive or nested folder.', icon: Upload },
      { value: 'adopt', title: 'Adopt a local directory', description: 'Use an existing allowlisted host directory.', icon: FolderInput },
    ].map(choice => <label key={choice.value} className={mode === choice.value ? 'selected' : ''}><input type="radio" name="source-mode" value={choice.value} checked={mode === choice.value} onChange={() => choose(choice.value as CreateInstance['mode'])} /><choice.icon size={23} aria-hidden="true" /><strong>{choice.title}</strong><span>{choice.description}</span></label>)}</fieldset>
    {operation.error && <ErrorNotice error={operation.error} />}
    {operation.data && <JobResult job={operation.data} />}
    {mode === 'import' && <Section title="Stage your server files" description="Uploading does not modify an instance. Review the server-side inspection before importing."><UploadPanel scope="new-instance" disabled={!allowed || operation.isPending} onReset={() => { setUpload(undefined); preview.reset() }} onComplete={completed => { setUpload(completed); preview.reset() }} />{upload && <Button busy={preview.isPending} onClick={() => { void preview.run('/imports/preview', { upload_id: upload.id }).catch(() => {}) }}>Inspect staged archive</Button>}</Section>}
    {mode === 'adopt' && <Section title="Inspect the local directory" description="Stop the existing server before adoption. The path must belong to the configured allowlist."><QueryState query={roots}>{data => <Notice>Allowlisted roots: {data.roots.length ? data.roots.map(root => <code key={root}>{root} </code>) : 'none configured; configure an allowlist on the panel host first'}</Notice>}</QueryState><SubmitForm onSubmit={() => preview.run('/adoptions/preview', { path }).then(() => {})}><Field label="Existing absolute directory"><input value={path} onChange={event => { setPath(event.target.value); preview.reset() }} required placeholder="/srv/minecraft/existing" disabled={!allowed} /></Field><Button type="submit" busy={preview.isPending} disabled={!allowed || !roots.data?.roots.length}>Inspect directory</Button></SubmitForm></Section>}
    {preview.error && <ErrorNotice error={preview.error} />}{mode !== 'create' && preview.data && <Section title="Source inspection"><ImportReport report={preview.data} />{preview.data.launchers.length === 0 && <Notice tone="warning">No supported launcher was detected. Supply a complete server archive or directory before continuing.</Notice>}</Section>}
    <Section title="Instance details" description="Runtime choices come from Fabric metadata. Imported and adopted launchers are selected from the inspected source.">
      <SubmitForm onSubmit={create} className="settings-form"><fieldset disabled={!allowed || operation.isPending}>
        <div className="form-grid"><Field label="Instance name"><input name="name" required maxLength={100} placeholder="Survival server" /></Field><Field label="Memory limit (MiB)"><input name="memory_mb" type="number" min={512} step={256} defaultValue={2048} required /></Field><Field label="Game port"><input name="port" type="number" min={1024} max={65535} defaultValue={25565} required /></Field><Field label="Local RCON port" hint="The source must already use a safe local RCON configuration when adopting."><input name="rcon_port" type="number" min={1024} max={65535} defaultValue={25575} required /></Field></div>
        {mode !== 'create' && <Field label="Detected server launcher"><select name="jar" required defaultValue=""><option value="" disabled>Choose a detected launcher</option>{preview.data?.launchers.map(launcher => <option key={launcher}>{launcher}</option>)}</select></Field>}
        <QueryState query={versions}>{available => <QueryState query={java} empty={<Notice tone="warning">No Java runtimes were detected. Configure an allowlisted compatible runtime on the host, then refresh.</Notice>}>{runtimes => <><RuntimeFields versions={available} java={runtimes} onGameChange={setGame} installer={mode === 'create'} />{runtimes.some(runtime => runtime.error) && <Notice tone="warning">Some configured Java runtimes could not be inspected. Check the host configuration.</Notice>}</>}</QueryState>}</QueryState>
        <CheckField name="eula" required>I have read and agree to the <a href="https://www.minecraft.net/eula" target="_blank" rel="noreferrer">Minecraft EULA</a> for this server.</CheckField>
        {mode !== 'create' && <CheckField name="source_confirm" required>I have permission to use these files and have reviewed the inspection above.</CheckField>}
        <div className="form-actions"><Button type="submit" variant="primary" busy={operation.isPending} disabled={operation.isSuccess || !versions.data || !java.data?.some(runtime => !runtime.error) || (mode !== 'create' && !preview.data?.launchers.length)}><Server size={16} />{mode === 'adopt' ? 'Validate and adopt directory' : mode === 'import' ? 'Validate and import server' : 'Create Fabric server'}</Button><span>Follow the accepted job to confirm completion.</span></div>
      </fieldset></SubmitForm>
    </Section>
  </>
}
