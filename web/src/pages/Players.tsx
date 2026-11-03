import { useState } from 'react'
import { Users } from 'lucide-react'
import { canAdmin, canOperate, instancePath, useAPI, useOperation, type Instance, type Players as PlayerData, type User } from '../api'
import { Button, DataTable, Empty, Field, JsonDetails, Notice, OperationFeedback, PageHeading, QueryState, ReadOnly, Section, Status, SubmitForm, useConfirm } from '../components'

export function playerName(record: unknown): string | undefined {
  if (typeof record === 'string') return record
  if (record && typeof record === 'object' && 'name' in record && typeof record.name === 'string') return record.name
  return undefined
}
export function Players({ instance, user }: { instance: Instance; user: User }) {
  const players = useAPI<PlayerData>(instancePath(instance.id, '/players'), { interval: 10_000 })
  const operation = useOperation()
  const [action, setAction] = useState('whitelist_add')
  const confirm = useConfirm()
  const allowed = canOperate(user, instance.id)
  const admin = canAdmin(user)
  async function perform(data: FormData) {
    const player = String(data.get('player') ?? '').trim()
    const value = String(data.get('value') ?? '').trim()
    if (action === 'op_level' && (!admin || !['1', '2', '3', '4'].includes(value))) throw new Error('Choose an operator level from 1 to 4.')
    if (['op', 'deop', 'op_level', 'ban', 'ban_ip', 'pardon_ip'].includes(action) && !await confirm({ title: action === 'op_level' ? 'Set the existing operator level?' : `Confirm ${action.replaceAll('_', ' ')}?`, body: <p>This changes access or privileges for <strong>{player}</strong>. {action === 'op_level' && <>Level {value} is applied only to an existing operator while the server is already stopped. </>}The action will be recorded in the audit history.</p>, action: 'Confirm player action', danger: true })) return
    await operation.run(instancePath(instance.id, '/players'), { action, ...(player ? { player } : {}), ...(value ? { value } : {}) })
  }
  return <><PageHeading title="Players" description="Online players, access lists, and server-side moderation." /><OperationFeedback operation={operation} />{!allowed && <ReadOnly />}
    <Section title="Player records"><QueryState query={players}>{data => {
      const whitelist = data.whitelist.map(playerName).filter((name): name is string => !!name)
      const operators = data.operators.map(playerName).filter((name): name is string => !!name)
      const bans = data.bans.map(playerName).filter((name): name is string => !!name)
      const names = [...new Set([...data.online, ...whitelist, ...operators, ...bans])].sort((a, b) => a.localeCompare(b))
      return <>{names.length ? <DataTable label="Player access records" rows={names} rowKey={name => name} columns={[
        { header: 'Player', render: name => <strong>{name}</strong> },
        { header: 'Presence', render: name => <Status value={data.online.includes(name) ? 'online' : 'offline'} /> },
        { header: 'Whitelist', render: name => whitelist.includes(name) ? 'Listed' : 'Not listed' },
        { header: 'Operator', render: name => operators.includes(name) ? 'Granted' : 'Not granted' },
        { header: 'Ban', render: name => bans.includes(name) ? <Status value="banned" /> : 'Not listed' },
      ]} /> : <Empty title="No player records">Players appear when they connect or are added to an access list.</Empty>}<JsonDetails label="Exact access-list records" value={{ whitelist: data.whitelist, operators: data.operators, bans: data.bans }} />{admin && data.ip_bans && <JsonDetails label="IP ban records (administrator only)" value={data.ip_bans} />}</>
    }}</QueryState></Section>
    {allowed && <Section title="Player action" description="Targets are validated on the server. Operator grants and IP moderation require administrator access."><SubmitForm onSubmit={perform}><fieldset disabled={operation.isPending}><div className="form-grid"><Field label="Action"><select value={action} onChange={event => setAction(event.target.value)}><option value="whitelist_add">Add to whitelist</option><option value="whitelist_remove">Remove from whitelist</option><option value="kick">Kick player</option><option value="ban">Ban player</option><option value="pardon">Pardon player</option><option value="say">Broadcast a message</option>{admin && <><option value="op">Grant operator</option><option value="deop">Revoke operator</option><option value="op_level">Set existing operator level (offline)</option><option value="ban_ip">Ban an IP address</option><option value="pardon_ip">Pardon an IP address</option></>}</select></Field>{action !== 'say' && <Field label={action.endsWith('_ip') ? 'IP address' : 'Player name'}><input name="player" required pattern={action.endsWith('_ip') ? undefined : '[A-Za-z0-9_]{1,16}'} maxLength={action.endsWith('_ip') ? 45 : 16} /></Field>}{action === 'op_level' ? <Field label="Operator level"><select name="value" defaultValue="" required><option value="" disabled>Choose a level</option>{[1, 2, 3, 4].map(level => <option key={level} value={level}>{level}</option>)}</select></Field> : <Field label={action === 'say' ? 'Message' : 'Reason / value (optional)'}><input name="value" required={action === 'say'} maxLength={500} /></Field>}</div>{action === 'op_level' && <Notice tone="warning">The server must already be stopped. This changes only an existing operator record and does not grant operator. Grant or revoke operator while RCON is ready.</Notice>}<Button type="submit" variant="primary" busy={operation.isPending}><Users size={16} />Queue player action</Button></fieldset></SubmitForm></Section>}
    <Notice>Presence reflects the latest server observation. Access-list membership is not a claim that a player is currently connected.</Notice>
  </>
}
