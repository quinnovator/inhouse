// Copies the built client into internal/console/dist, where inhoused
// embeds it. The directory's .gitkeep stays so a checkout without a console
// build still compiles (and serves a page saying the console isn't built).
import { cpSync, readdirSync, rmSync } from 'node:fs'
import { join } from 'node:path'

const from = new URL('../dist/client/', import.meta.url).pathname
const to = new URL('../../internal/console/dist/', import.meta.url).pathname

for (const name of readdirSync(to)) {
  if (name !== '.gitkeep') rmSync(join(to, name), { recursive: true })
}
for (const name of readdirSync(from)) {
  if (name.startsWith('.')) continue // e.g. .vite build metadata
  cpSync(join(from, name), join(to, name), { recursive: true })
}
console.log(`embedded ${from} into ${to}`)
