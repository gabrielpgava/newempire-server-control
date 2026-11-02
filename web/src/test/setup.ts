import '@testing-library/jest-dom/vitest'
import { afterEach, vi } from 'vitest'
import { cleanup, configure } from '@testing-library/react'

// Multi-step interactions also run in the two-CPU Linux sandbox, not as performance benchmarks.
configure({ asyncUtilTimeout: 5_000 })

Object.defineProperty(HTMLDialogElement.prototype, 'showModal', { configurable: true, value() { this.setAttribute('open', '') } })
Object.defineProperty(HTMLDialogElement.prototype, 'close', { configurable: true, value() { this.removeAttribute('open') } })
Element.prototype.scrollIntoView = vi.fn()
window.scrollTo = vi.fn()
afterEach(() => { cleanup(); vi.unstubAllGlobals(); window.location.hash = ''; localStorage.clear() })
