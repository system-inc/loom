import { cloudflareTest } from '@cloudflare/vitest-plugin';
import { defineConfig } from 'vitest/config';

// loom's tests, against its own Worker and bindings (pipeline.jsonc); loom-runs's are in vitest.config.ts.
export default defineConfig({
    plugins: [
        cloudflareTest({
            wrangler: { configPath: './pipeline.jsonc' },
            miniflare: {
                // The pinned vector's secret from docs/protocol.md; the real one never leaves the deployed Workers.
                bindings: { LOOM_TOKEN_SECRET: 'loom-test-secret' },
            },
        }),
    ],
    test: { include: ['test/pipeline/**/*.test.ts'] },
});
