import { useEffect, useRef, useState } from 'react'
import { Upload as TusUpload } from 'tus-js-client'
import { Zip, ZipPassThrough } from 'fflate'
import { FileArchive, FolderUp, Pause, Play, Upload, X } from 'lucide-react'
import { API, errorMessage, formatBytes, getCSRFToken, validRelativePath } from './api'
import { Button, ErrorNotice, Field, Notice } from './components'

export interface CompletedUpload { id: string; filename: string; bytes: number }
export function folderEntryPath(file: File) {
  const path = file.webkitRelativePath || file.name
  if (!validRelativePath(path) || path === '.') throw new Error(`The folder contains an unsafe path: ${path}`)
  return path
}

export async function packageFolder(files: File[], onProgress: (bytes: number) => void, signal?: AbortSignal): Promise<File> {
  if (!files.length) throw new Error('Select a folder containing at least one file.')
  // shortcut: browser folder packaging is limited to 512 MiB, use a prebuilt archive for larger imports.
  const total = files.reduce((sum, file) => sum + file.size, 0)
  if (total > 512 * 1024 * 1024 || files.length > 20_000) throw new Error('This folder is too large to package in the browser. Create a ZIP or tar.gz archive locally, then upload that archive.')
  files = [...files].sort((a, b) => folderEntryPath(a) < folderEntryPath(b) ? -1 : folderEntryPath(a) > folderEntryPath(b) ? 1 : 0)
  const paths = files.map(folderEntryPath)
  if (new Set(paths).size !== paths.length) throw new Error('The selected folder contains duplicate file paths.')
  const commonRoot = paths.every(path => path.includes('/') && path.split('/')[0] === paths[0].split('/')[0])
  const chunks: BlobPart[] = []
  let zipError: Error | null = null
  const zip = new Zip((error, data) => {
    if (error) zipError = error
    else chunks.push(new Uint8Array(data).buffer)
  })
  let completed = 0
  for (let index = 0; index < files.length; index++) {
    signal?.throwIfAborted()
    const path = commonRoot ? paths[index].slice(paths[index].indexOf('/') + 1) : paths[index]
    const entry = new ZipPassThrough(path)
    entry.mtime = new Date(Math.max(Date.UTC(1980, 0, 1), files[index].lastModified))
    zip.add(entry)
    const reader = files[index].stream().getReader()
    try {
      while (true) {
        signal?.throwIfAborted()
        const { done, value } = await reader.read()
        if (done) break
        entry.push(value)
        completed += value.byteLength
        onProgress(completed)
      }
      entry.push(new Uint8Array(), true)
    } finally { await reader.cancel() }
    if (zipError) throw zipError
  }
  zip.end()
  if (zipError) throw zipError
  const name = commonRoot ? paths[0].split('/')[0] : 'folder-import'
  return new File(chunks, `${name}.zip`, { type: 'application/zip', lastModified: files.reduce((latest, file) => Math.max(latest, file.lastModified), 0) })
}

export function validateUploadURL(value: string, origin = window.location.origin) {
  const url = new URL(value, origin)
  if (url.origin !== origin || url.username || url.password || !url.pathname.startsWith(`${API}/uploads/`) || url.search || url.hash) throw new Error('The server returned an invalid upload location.')
  return url
}

export function UploadPanel({ onComplete, onReset, disabled, scope, modpack = false }: { onComplete: (upload: CompletedUpload) => void; onReset?: () => void; disabled?: boolean; scope: string; modpack?: boolean }) {
  const [mode, setMode] = useState<'archive' | 'folder'>('archive')
  const [files, setFiles] = useState<File[]>([])
  const [state, setState] = useState<'idle' | 'packaging' | 'uploading' | 'paused' | 'complete'>('idle')
  const [bytes, setBytes] = useState(0)
  const [total, setTotal] = useState(0)
  const [error, setError] = useState<unknown>(null)
  const upload = useRef<TusUpload | null>(null)
  const packaging = useRef<AbortController | null>(null)
  const mounted = useRef(true)
  const fileRef = useRef<HTMLInputElement>(null)
  useEffect(() => { mounted.current = true; return () => { mounted.current = false; packaging.current?.abort(); void upload.current?.abort() } }, [])
  const busy = state === 'packaging' || state === 'uploading' || state === 'paused'
  async function start() {
    setError(null)
    try {
      if (!files.length) throw new Error('Choose an archive or folder first.')
      let file = files[0]
      if (modpack && !/\.mrpack$/i.test(file.name)) throw new Error('Choose a server modpack with the .mrpack extension. The server validates its contents before applying it.')
      if (mode === 'folder') {
        setState('packaging'); setBytes(0); setTotal(files.reduce((sum, item) => sum + item.size, 0))
        packaging.current = new AbortController()
        file = await packageFolder(files, value => { if (mounted.current) setBytes(value) }, packaging.current.signal)
      }
      if (!mounted.current) return
      setState('uploading'); setBytes(0); setTotal(file.size)
      const current = new TusUpload(file, {
        endpoint: `${API}/uploads/`,
        chunkSize: 8 * 1024 * 1024,
        retryDelays: [0, 1_000, 3_000, 5_000],
        metadata: { filename: file.name, filetype: file.type || 'application/octet-stream' },
        fingerprint: async () => ['nesc', scope, file.name, file.size, file.lastModified].join('-'),
        removeFingerprintOnSuccess: true,
        onBeforeRequest: request => { validateUploadURL(request.getURL()); request.setHeader('X-CSRF-Token', getCSRFToken()); const xhr = request.getUnderlyingObject(); if (xhr instanceof XMLHttpRequest) xhr.withCredentials = true },
        onProgress: (uploaded, size) => { if (mounted.current) { setBytes(uploaded); setTotal(size) } },
        onError: cause => { if (mounted.current) { setError(cause); setState('paused') } },
        onSuccess: () => {
          if (!mounted.current) return
          try {
            const url = validateUploadURL(current.url!)
            const id = url.pathname.split('/').filter(Boolean).at(-1)!
            setState('complete'); onComplete({ id, filename: file.name, bytes: file.size })
          } catch (cause) { setError(cause); setState('paused') }
        },
      })
      upload.current = current
      const previous = await current.findPreviousUploads()
      if (!mounted.current) return
      if (previous.length) current.resumeFromPreviousUpload(previous[0])
      current.start()
    } catch (cause) {
      if (mounted.current) { if (!(cause instanceof DOMException && cause.name === 'AbortError')) setError(cause); setState('idle') }
    }
  }
  async function cancel() {
    packaging.current?.abort()
    try { await upload.current?.abort(true); upload.current = null; setState('idle'); setBytes(0); setError(null); onReset?.() } catch (cause) { setError(cause) }
  }
  return <div className="upload-panel">
    {!modpack && <fieldset className="inline-options" disabled={busy || disabled}><legend>Upload source</legend><label><input type="radio" name={`upload-${scope}`} checked={mode === 'archive'} onChange={() => { setMode('archive'); setFiles([]); setState('idle'); onReset?.() }} /><FileArchive size={17} />Archive</label><label><input type="radio" name={`upload-${scope}`} checked={mode === 'folder'} onChange={() => { setMode('folder'); setFiles([]); setState('idle'); onReset?.() }} /><FolderUp size={17} />Folder</label></fieldset>}
    <Field label={modpack ? 'Choose a server .mrpack file' : mode === 'archive' ? 'Choose a ZIP or tar.gz archive' : 'Choose a folder'} hint={mode === 'folder' ? 'Nested paths are preserved. Folders up to 512 MiB are packaged locally as ZIP; empty directories are not included.' : 'Uploads are staged separately from the instance and can resume after interruption.'}>
      <input ref={fileRef} key={mode} type="file" disabled={busy || disabled} accept={modpack ? '.mrpack' : mode === 'archive' ? '.zip,.tar.gz,.tgz' : undefined} {...(mode === 'folder' ? { webkitdirectory: '', directory: '', multiple: true } : {})} onChange={event => { setFiles(Array.from(event.target.files ?? [])); setState('idle'); setError(null); onReset?.() }} />
    </Field>
    {files.length > 0 && <p className="muted">{files.length === 1 ? files[0].name : `${files.length.toLocaleString()} files selected`} · {formatBytes(files.reduce((sum, file) => sum + file.size, 0))}</p>}
    {!!error && <ErrorNotice error={new Error(errorMessage(error))} />}
    {(busy || state === 'complete') && <div className="upload-progress" role="status"><div><strong>{state === 'packaging' ? 'Packaging folder' : state === 'paused' ? 'Upload paused' : state === 'complete' ? 'Upload staged' : 'Uploading'}</strong><span>{formatBytes(bytes)} / {formatBytes(total)}</span></div><progress value={bytes} max={total || 1} aria-label={state === 'packaging' ? 'Folder packaging' : 'Archive upload'} /></div>}
    {state === 'complete' ? <Notice tone="success">The upload is staged, not installed. Review the import before applying it.</Notice> : <div className="actions">
      {state === 'idle' && <Button disabled={disabled || !files.length} onClick={() => { void start() }}><Upload size={16} />Upload to staging</Button>}
      {state === 'uploading' && <Button onClick={() => { void upload.current?.abort().then(() => setState('paused')).catch(setError) }}><Pause size={16} />Pause</Button>}
      {state === 'paused' && <Button onClick={() => { setError(null); setState('uploading'); upload.current?.start() }}><Play size={16} />Resume</Button>}
      {busy && <Button onClick={() => { void cancel() }}><X size={16} />Cancel upload</Button>}
    </div>}
  </div>
}
