import { cloneElement, createContext, isValidElement, useContext, useEffect, useId, useRef, useState, type ButtonHTMLAttributes, type FormEvent, type ReactElement, type ReactNode } from 'react'
import type { UseQueryResult } from '@tanstack/react-query'
import { AlertTriangle, Check, CheckCircle2, ChevronRight, LoaderCircle, RefreshCw, X } from 'lucide-react'
import { APIError, errorMessage, formatDate, type Job } from './api'

export function Button({ children, variant = 'secondary', busy, className = '', disabled, ...props }: ButtonHTMLAttributes<HTMLButtonElement> & { variant?: 'primary' | 'secondary' | 'danger' | 'ghost'; busy?: boolean }) {
  return <button className={`button button-${variant} ${className}`} disabled={disabled || busy} aria-busy={busy || undefined} {...props}>{busy && <LoaderCircle size={16} className="spin" aria-hidden="true" />}{children}</button>
}
export function Field({ label, hint, children, className = '' }: { label: string; hint?: string; children: ReactNode; className?: string }) {
  const id = useId()
  const control = isValidElement(children) ? cloneElement(children as ReactElement<{ id?: string; 'aria-describedby'?: string }>, { id, 'aria-describedby': hint ? `${id}-hint` : undefined }) : children
  return <div className={`field ${className}`}><label className="field-label" htmlFor={id}>{label}</label>{control}{hint && <span className="field-hint" id={`${id}-hint`}>{hint}</span>}</div>
}
export function CheckField({ children, ...props }: React.InputHTMLAttributes<HTMLInputElement> & { children: ReactNode }) {
  return <label className="check-field"><input type="checkbox" {...props} /><span>{children}</span></label>
}
export function ErrorNotice({ error, retry, stale = false }: { error: unknown; retry?: () => void; stale?: boolean }) {
  return <div className="notice notice-error" role="alert"><AlertTriangle size={19} aria-hidden="true" /><div><strong>{stale ? 'Could not refresh data' : 'Could not complete the request'}</strong><p>{errorMessage(error)}</p>{stale && <p>Showing previously loaded data; it may be out of date.</p>}{retry && <Button onClick={retry}><RefreshCw size={14} />Try again</Button>}</div></div>
}
export function Notice({ children, tone = 'info' }: { children: ReactNode; tone?: 'info' | 'warning' | 'success' }) {
  return <div className={`notice notice-${tone}`} role="status">{tone === 'success' ? <CheckCircle2 size={19} aria-hidden="true" /> : <AlertTriangle size={19} aria-hidden="true" />}<div>{children}</div></div>
}
export function Empty({ title, children, action }: { title: string; children?: ReactNode; action?: ReactNode }) {
  return <div className="empty"><h3>{title}</h3>{children && <p>{children}</p>}{action}</div>
}
export function Loading({ label = 'Loading current data…' }: { label?: string }) {
  return <div className="loading" role="status"><LoaderCircle size={19} className="spin" aria-hidden="true" />{label}</div>
}
export function QueryState<T>({ query, children, empty }: { query: UseQueryResult<T, Error>; children: (data: T) => ReactNode; empty?: ReactNode }) {
  if (query.isPending) return <Loading />
  if (query.isError && (query.data === undefined || (query.error instanceof APIError && query.error.status === 401))) return <ErrorNotice error={query.error} retry={() => { void query.refetch() }} />
  return <>{query.isError && <ErrorNotice error={query.error} stale retry={() => { void query.refetch() }} />}{query.data === undefined || query.data === null || (Array.isArray(query.data) && query.data.length === 0) ? empty ?? <Empty title="Nothing to show yet">New records will appear here when available.</Empty> : children(query.data)}</>
}
export function PageHeading({ title, description, actions }: { title: string; description?: string; actions?: ReactNode }) {
  return <header className="page-heading"><div><h1>{title}</h1>{description && <p>{description}</p>}</div>{actions && <div className="actions">{actions}</div>}</header>
}
export function Section({ title, description, actions, children, className = '' }: { title: string; description?: string; actions?: ReactNode; children: ReactNode; className?: string }) {
  return <section className={`section ${className}`}><div className="section-heading"><div><h2>{title}</h2>{description && <p>{description}</p>}</div>{actions && <div className="actions">{actions}</div>}</div>{children}</section>
}
export function DataTable<T>({ label, columns, rows, rowKey }: { label: string; columns: { header: string; render: (row: T) => ReactNode; className?: string }[]; rows: T[]; rowKey: (row: T) => string }) {
  return <div className="table-scroll" role="region" aria-label={label} tabIndex={0}><table><caption className="sr-only">{label}</caption><thead><tr>{columns.map(column => <th key={column.header} scope="col" className={column.className}>{column.header}</th>)}</tr></thead><tbody>{rows.map(row => <tr key={rowKey(row)}>{columns.map(column => <td key={column.header} className={column.className}>{column.render(row)}</td>)}</tr>)}</tbody></table></div>
}
export function Status({ value, label }: { value: string; label?: string }) {
  const normalized = value.toLowerCase()
  const tone = ['running', 'ready', 'succeeded', 'completed', 'verified', 'online', 'active', 'enabled'].includes(normalized) ? 'good' : ['failed', 'error', 'banned'].includes(normalized) ? 'bad' : ['pending', 'queued', 'starting', 'stopping', 'cancelling', 'degraded', 'staged'].includes(normalized) ? 'warn' : 'neutral'
  return <span className={`status status-${tone}`}><span aria-hidden="true" />{label ?? value.replaceAll('_', ' ')}</span>
}
export function JobResult({ job }: { job?: Job }) {
  if (!job) return null
  if (!job.id) return <Notice>The server accepted the request. Refresh the related records to check the result.</Notice>
  return <Notice tone={job.state === 'failed' ? 'warning' : 'info'}><strong>Job {job.state || 'accepted'}</strong> <a href={`#/jobs/${encodeURIComponent(job.id)}`}>Follow job {job.id.slice(0, 8)}<ChevronRight size={14} /></a>{job.step && <p>{job.step}</p>}</Notice>
}
export function OperationFeedback({ operation }: { operation: { error: unknown; data?: Job; isSuccess: boolean } }) {
  return <>{!!operation.error && <ErrorNotice error={operation.error} />}{operation.isSuccess && <JobResult job={operation.data} />}</>
}
export function JsonDetails({ value, label = 'Technical details' }: { value: unknown; label?: string }) {
  return <details className="technical-details"><summary>{label}</summary><pre>{JSON.stringify(value, null, 2)}</pre></details>
}
export function PlanView({ plan }: { plan: Record<string, unknown> }) {
  return <div className="plan-view">{typeof plan.summary === 'string' && <p className="lead">{plan.summary}</p>}{Array.isArray(plan.warnings) && plan.warnings.length > 0 && <Notice tone="warning"><ul>{plan.warnings.map((warning, index) => <li key={index}>{String(warning)}</li>)}</ul></Notice>}<JsonDetails value={plan} label="Review the complete plan" /></div>
}
export function SubmitForm({ onSubmit, children, className = '' }: { onSubmit: (data: FormData, form: HTMLFormElement) => void | Promise<void>; children: ReactNode; className?: string }) {
  const [error, setError] = useState<unknown>(null)
  function submit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault(); setError(null); const form = event.currentTarget
    void Promise.resolve().then(() => onSubmit(new FormData(form), form)).catch(cause => { if (!(cause instanceof APIError)) setError(cause) })
  }
  return <form className={className} onSubmit={submit}>{!!error && <ErrorNotice error={error} />}{children}</form>
}
export function ReadOnly({ admin = false }: { admin?: boolean }) {
  return <p className="read-only">{admin ? 'An administrator role is required to change these settings.' : 'Your role has read-only access to this instance.'}</p>
}

type Confirmation = { title: string; body: ReactNode; action: string; danger?: boolean; verify?: string }
const ConfirmContext = createContext<(options: Confirmation) => Promise<boolean>>(() => Promise.resolve(false))
export function useConfirm() { return useContext(ConfirmContext) }
export function ConfirmProvider({ children }: { children: ReactNode }) {
  const [request, setRequest] = useState<(Confirmation & { resolve: (value: boolean) => void }) | null>(null)
  const [typed, setTyped] = useState('')
  const dialog = useRef<HTMLDialogElement>(null)
  const returnFocus = useRef<HTMLElement | null>(null)
  const heading = useId()
  const description = useId()
  useEffect(() => { if (request) { setTyped(''); dialog.current?.showModal() } else { dialog.current?.close(); returnFocus.current?.focus() } }, [request])
  function close(value: boolean) { request?.resolve(value); setRequest(null) }
  return <ConfirmContext value={options => new Promise(resolve => { returnFocus.current = document.activeElement instanceof HTMLElement ? document.activeElement : null; setRequest({ ...options, resolve }) })}>{children}<dialog ref={dialog} className="confirm-dialog" aria-labelledby={heading} aria-describedby={description} onCancel={event => { event.preventDefault(); close(false) }}>
    {request && <><div className="dialog-heading"><h2 id={heading}>{request.title}</h2><Button variant="ghost" aria-label="Cancel" onClick={() => close(false)}><X size={20} /></Button></div><div id={description} className="dialog-body">{request.body}</div>{request.verify && <Field label={`Type “${request.verify}” to confirm`}><input autoComplete="off" value={typed} onChange={event => setTyped(event.target.value)} /></Field>}<div className="dialog-actions"><Button autoFocus onClick={() => close(false)}>Cancel</Button><Button variant={request.danger ? 'danger' : 'primary'} disabled={!!request.verify && typed !== request.verify} onClick={() => close(true)}>{request.action}</Button></div></>}
  </dialog></ConfirmContext>
}
export function Timeline({ stages }: { stages: { label: string; detail: string; complete: boolean; current?: boolean }[] }) {
  return <ol className="readiness" aria-label="Instance readiness">{stages.map(stage => <li key={stage.label} className={stage.complete ? 'complete' : stage.current ? 'current' : ''}><span className="readiness-mark" aria-hidden="true">{stage.complete ? <Check size={14} /> : <span />}</span><div><strong>{stage.label}</strong><small>{stage.detail}</small></div></li>)}</ol>
}
export function DateText({ value }: { value?: string | number }) { return <time dateTime={typeof value === 'number' ? new Date(value * 1000).toISOString() : value}>{formatDate(value)}</time> }
