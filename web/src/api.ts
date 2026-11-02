import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'

export const API = '/api/v1'
export type Role = 'owner' | 'admin' | 'operator' | 'viewer'
export interface User { id: string; username: string; role: Role; two_factor?: boolean; instance_ids?: string[]; created_at?: number }
export interface Session { user: User; csrf_token: string }
export interface Instance {
  id: string; name: string; root: string; java_path: string; jar: string
  minecraft_version: string; loader_version: string; memory_mb: number; port: number
  rcon_port: number; eula: boolean; adopted: boolean; created_at: string
}
export interface RuntimeStatus {
  state: string; ready: boolean; pid?: number; uptime_seconds: number
  memory_bytes: number; cpu_seconds: number; players: string[]; detail?: string; disk_free_bytes?: number
}
export interface Job {
  id: string; instance_id?: string; kind: string; state: string; progress?: number
  step?: string; error?: string; created_at: number; updated_at?: number; payload?: unknown
  cancel_requested?: boolean; cancellable?: boolean; attempts?: number; plan?: unknown; result?: { instance?: Instance; [key: string]: unknown }
}
export interface JobDetail { job: Job; steps: { time: number; progress: number; message: string }[] }
export interface LogLine { time: string; message: string }
export interface FileEntry { name: string; path: string; is_dir: boolean; size: number; modified_at?: string; mode?: string }
export interface Backup { id: string; instance_id: string; created_at: number; size: number; sha256: string; note: string; verified: boolean; local?: boolean; offsite?: { state: string; object?: { verification?: string } } }
export interface World { name: string; path: string; active: boolean; dimensions: string[] }
export interface Worlds { active: string; worlds: World[] }
export interface Players { online: string[]; whitelist: unknown[]; operators: unknown[]; bans: unknown[]; ip_bans?: unknown[] }
export interface Mod {
  filename: string; project_id?: string; name?: string; version?: string; version_id?: string
  sha512: string; managed: boolean; pinned: boolean; update_available: boolean; reason?: string
}
export interface ModSearchResult { project_id: string; title: string; description: string; slug: string; server_side?: string }
export interface ModPolicy { mode: string; interval_hours: number; window_start: string; window_end: string; timezone: string; postpone_players: boolean }
export interface FabricVersions { game_versions: string[]; loader_versions: string[]; installer_versions: string[] }
export interface JavaRuntime { path: string; version: string; major: number; error?: string }
export interface Plan { plan_hash: string; summary?: string; warnings: string[]; changes?: string[]; java_required?: number; [key: string]: unknown }
export interface ModPlan extends Plan { actions: { project_id: string; name: string; from_version: string; to_version: string; filename: string; reason: string }[]; blocked: string[] }
export interface ImportPreview { preview_hash: string; files: { path: string; size: number }[]; total_bytes: number; file_count: number; warnings: string[]; launchers: string[]; worlds: string[]; minecraft_version?: string; loader_version?: string; conflicts?: string[] }
export interface RestorePreview { files: { path: string; size: number }[]; total_bytes: number; file_count: number; warnings: string[] }
export interface Schedule { id: string; instance_id: string; kind: string; hour: number; minute: number; weekdays: number[]; timezone: string; enabled: boolean; next_run?: number; last_job_id?: string }
export interface AuditEntry { id: string; time: number; user_id: string; action: string; instance_id?: string; detail?: string }
export interface PanelSettings { public_url: string; version: string; runtime_provider: string; adopt_roots: string[]; java_paths: string[]; notifications: { configured: boolean; enabled: boolean }; storage: { configured: boolean; enabled: boolean }; backup_retention: { enabled: boolean; keep_last: number }; [key: string]: unknown }
export interface UpdatePolicy { mode: string; channel: string; interval_hours: number; window_start: string; window_end: string; timezone: string; postpone_until?: number }
export interface UpdateState {
  current_version: string; repository: string; available: null | { version: string; notes: string; size: number; prerelease: boolean }
  state: string; detail?: string; history: unknown[]; policy: UpdatePolicy
}
export interface CreateInstance {
  name: string; mode: 'create' | 'import' | 'adopt'; minecraft_version: string
  loader_version: string; java_path: string; memory_mb: number; port: number
  rcon_port: number; eula: boolean; path?: string; upload_id?: string; preview_hash?: string; jar?: string; installer_version?: string
}

let csrfToken = ''
export function setSession(session?: Session) { csrfToken = session?.csrf_token ?? '' }
export function getCSRFToken() { return csrfToken }

export class APIError extends Error {
  constructor(message: string, public status: number) { super(message); this.name = 'APIError' }
}

export async function request<T>(path: string, options: RequestInit = {}): Promise<T> {
  if (!path.startsWith('/') || path.startsWith('//')) throw new Error('An API-relative path is required.')
  const headers = new Headers(options.headers)
  headers.set('Accept', 'application/json')
  if (options.body && !(options.body instanceof FormData)) headers.set('Content-Type', 'application/json')
  if (options.method && !['GET', 'HEAD', 'OPTIONS'].includes(options.method.toUpperCase()) && csrfToken) headers.set('X-CSRF-Token', csrfToken)
  const response = await fetch(`${API}${path}`, { ...options, headers, credentials: 'same-origin', redirect: 'error' })
  if (!response.ok) {
    let message = `Request failed (${response.status}). Please try again.`
    try { const body = await response.json(); if (typeof body.error === 'string') message = body.error } catch { /* Non-JSON errors keep their HTTP status. */ }
    if (response.status === 401 && path !== '/auth/session' && path !== '/auth/login') window.dispatchEvent(new Event('session-expired'))
    throw new APIError(message, response.status)
  }
  if (response.status === 204) return undefined as T
  const contentType = response.headers.get('content-type') ?? ''
  if (!contentType.includes('application/json')) throw new APIError('The server returned an unexpected response. Refresh the page or check the panel logs.', response.status)
  return response.json() as Promise<T>
}

export function useAPI<T>(path: string, options: { enabled?: boolean; interval?: number; keepPreviousData?: boolean } = {}) {
  return useQuery({ queryKey: [path], queryFn: ({ signal }) => request<T>(path, { signal }), enabled: options.enabled, refetchInterval: options.interval, retry: false, placeholderData: options.keepPreviousData ? previous => previous : undefined })
}

export function useOperation<T = Job>() {
  const client = useQueryClient()
  const mutation = useMutation({
    mutationFn: ({ path, body, method = 'POST' }: { path: string; body?: unknown; method?: string }) => request<T>(path, { method, body: body === undefined ? undefined : JSON.stringify(body) }),
    onSuccess: () => { void client.invalidateQueries() },
  })
  return { ...mutation, run: (path: string, body?: unknown, method = 'POST') => mutation.mutateAsync({ path, body, method }) }
}

export function instancePath(id: string, suffix = '') { return `/instances/${encodeURIComponent(id)}${suffix}` }
export function downloadURL(id: string, path: string) { return `${API}${instancePath(id, `/file?path=${encodeURIComponent(path)}`)}` }
export function canOperate(user: User, instanceID?: string) {
  if (!['owner', 'admin', 'operator'].includes(user.role)) return false
  return user.role !== 'operator' || (!!instanceID && (!user.instance_ids || user.instance_ids.includes(instanceID)))
}
export function canAdmin(user: User) { return user.role === 'owner' || user.role === 'admin' }
export function isActiveJob(job: Job) { return ['pending', 'queued', 'running', 'cancelling'].includes(job.state) }
export function errorMessage(error: unknown) { return error instanceof Error ? error.message : 'An unexpected error occurred. Please try again.' }
export function formatBytes(bytes?: number) {
  if (bytes === undefined || !Number.isFinite(bytes)) return 'Not reported'
  if (bytes === 0) return '0 B'
  const unit = Math.min(Math.floor(Math.log(Math.abs(bytes)) / Math.log(1024)), 4)
  return `${(bytes / 1024 ** unit).toLocaleString(undefined, { maximumFractionDigits: unit ? 1 : 0 })} ${['B', 'KiB', 'MiB', 'GiB', 'TiB'][unit]}`
}
export function formatDate(date?: string | number) {
  if (date === undefined || date === '') return 'Not reported'
  const parsed = new Date(typeof date === 'number' ? date * 1000 : date)
  return Number.isNaN(parsed.getTime()) ? 'Not reported' : parsed.toLocaleString()
}
export function validRelativePath(path: string) {
  return !!path && !path.startsWith('/') && !path.includes('\\') && !path.includes('\0') && !path.split('/').some(part => part === '..' || part === '')
}
