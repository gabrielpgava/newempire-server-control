import { Download, Globe, Plus } from 'lucide-react'
import { API, canAdmin, instancePath, useAPI, useOperation, type Instance, type User, type Worlds as WorldData } from '../api'
import { Button, DataTable, Empty, Field, Notice, OperationFeedback, PageHeading, QueryState, ReadOnly, Section, Status, SubmitForm, useConfirm } from '../components'

export function Worlds({ instance, user }: { instance: Instance; user: User }) {
  const worlds = useAPI<WorldData>(instancePath(instance.id, '/worlds'))
  const operation = useOperation()
  const confirm = useConfirm()
  const allowed = canAdmin(user)
  async function mutate(kind: string, name: string) {
    if (await confirm({ title: kind === 'activate' ? `Activate ${name}?` : `Create ${name}?`, body: <p>{kind === 'activate' ? 'The server will change its active world after passing safety checks and a graceful shutdown. Existing worlds are retained.' : 'This prepares an empty world directory. Minecraft generates the world on its next start. No existing world is replaced.'}</p>, action: kind === 'activate' ? 'Activate world' : 'Create world', danger: kind === 'activate' })) await operation.run(instancePath(instance.id, '/worlds'), { operation: kind, name, confirm: true }).catch(() => {})
  }
  return <><PageHeading title="Worlds" description="Keep world selection explicit. Content failures never trigger automatic world rollback." /><OperationFeedback operation={operation} />{!allowed && <ReadOnly admin />}
    <Section title="World inventory"><QueryState query={worlds}>{data => data.worlds.length ? <DataTable label="World directories" rows={data.worlds} rowKey={world => world.path} columns={[
      { header: 'World', render: world => <span className="file-name"><Globe size={17} /><strong>{world.name}</strong></span> },
      { header: 'State', render: world => <Status value={world.active ? 'active' : 'inactive'} /> },
      { header: 'Dimensions', render: world => world.dimensions.length ? world.dimensions.join(', ') : 'Not reported' },
      { header: 'Path', render: world => <code>{world.path}</code> },
      { header: 'Actions', render: world => <div className="row-actions"><a className="button button-ghost" href={`${API}${instancePath(instance.id, `/export?path=${encodeURIComponent(world.path)}`)}`}><Download size={15} />Export</a>{allowed && !world.active && <Button disabled={operation.isPending} onClick={() => { void mutate('activate', world.name) }}>Activate</Button>}</div> },
    ]} /> : <Empty title="No worlds detected">Minecraft creates the selected world when it starts. Existing worlds can be imported from Files.</Empty>}</QueryState></Section>
    {allowed && <Section title="Prepare a new world" description="The name must be a safe single directory name."><SubmitForm onSubmit={data => mutate('create', String(data.get('name')))}><div className="inline-form"><Field label="World name"><input name="name" required maxLength={100} pattern="[A-Za-z0-9_-]+" placeholder="new-world" /></Field><Button type="submit" busy={operation.isPending}><Plus size={16} />Create world directory</Button></div></SubmitForm></Section>}
    <Notice>To import a world, use <a href={`#/instances/${instance.id}/files`}>Files → Import</a>. The import preview identifies affected paths before applying changes.</Notice>
  </>
}
