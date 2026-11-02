import { unzipSync, strFromU8 } from 'fflate'
import { describe, expect, it } from 'vitest'
import { folderEntryPath, packageFolder, validateUploadURL } from '../uploads'

function file(path: string, content: string) {
  const data = new TextEncoder().encode(content)
  const result = new File([data], path.split('/').at(-1)!, { lastModified: 1_793_577_600_000 })
  Object.defineProperty(result, 'webkitRelativePath', { value: path })
  Object.defineProperty(result, 'stream', { value: () => new ReadableStream({ start(controller) { controller.enqueue(data); controller.close() } }) })
  return result
}
function bytes(blob: Blob) { return new Promise<Uint8Array>((resolve, reject) => { const reader = new FileReader(); reader.onload = () => resolve(new Uint8Array(reader.result as ArrayBuffer)); reader.onerror = reject; reader.readAsArrayBuffer(blob) }) }

describe('staged resumable folder imports', () => {
  it('retains nested names and strips only the common selected directory', async () => {
    const archive = await packageFolder([file('server/world/level.dat', 'world'), file('server/config/options.txt', 'options')], () => {})
    const entries = unzipSync(await bytes(archive))
    expect(Object.keys(entries)).toEqual(['config/options.txt', 'world/level.dat'])
    expect(strFromU8(entries['world/level.dat'])).toBe('world')
  })
  it('produces stable bytes and fingerprint metadata when a folder is selected again', async () => {
    const files = [file('server/b.txt', 'b'), file('server/a.txt', 'a')]
    const first = await packageFolder(files, () => {}); const second = await packageFolder([...files].reverse(), () => {})
    expect(await bytes(first)).toEqual(await bytes(second)); expect(first.lastModified).toBe(second.lastModified)
  })
  it('rejects unsafe paths and duplicate names before staging', async () => {
    expect(() => folderEntryPath(file('server/../secret', 'bad'))).toThrow('unsafe path')
    await expect(packageFolder([file('server/a.txt', 'a'), file('server/a.txt', 'b')], () => {})).rejects.toThrow('duplicate')
  })
  it('rejects oversized browser packaging with an actionable archive alternative', async () => {
    const large = file('server/large.bin', '')
    Object.defineProperty(large, 'size', { value: 513 * 1024 * 1024 })
    await expect(packageFolder([large], () => {})).rejects.toThrow('Create a ZIP or tar.gz archive locally')
  })
  it('cancels packaging before reading source content', async () => {
    const controller = new AbortController(); controller.abort()
    await expect(packageFolder([file('server/a.txt', 'a')], () => {}, controller.signal)).rejects.toMatchObject({ name: 'AbortError' })
  })
  it('never sends CSRF headers to a resumed foreign upload location', () => {
    expect(validateUploadURL('/api/v1/uploads/upload-id', 'https://panel.test').origin).toBe('https://panel.test')
    for (const location of ['https://foreign.test/api/v1/uploads/id', '/api/v1/uploads/../settings', 'https://user:password@panel.test/api/v1/uploads/id', '/api/v1/uploads/id?redirect=other']) expect(() => validateUploadURL(location, 'https://panel.test')).toThrow('invalid upload location')
  })
})
