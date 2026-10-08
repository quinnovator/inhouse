import { defineConfig } from 'vitest/config'

// Unit tests cover plain modules, so they skip the Start plugin.
export default defineConfig({
  resolve: { tsconfigPaths: true },
  test: { include: ['src/**/*.test.ts'] },
})
