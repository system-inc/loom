import { defineConfig } from 'vitest/config';

// Tests that need Node rather than workerd: the pages' inline scripts compiled by V8 (node:vm), since workerd refuses
// code from strings. The Workers' own tests are in vitest.config.ts and vitest.pipeline.config.ts.
export default defineConfig({
    test: { include: ['test/node/**/*.test.ts'], environment: 'node' },
});
