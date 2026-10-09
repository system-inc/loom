import { cloudflareTest } from '@cloudflare/vitest-plugin';
import { configDefaults, defineConfig } from 'vitest/config';

export default defineConfig({
    plugins: [
        cloudflareTest({
            wrangler: { configPath: './wrangler.jsonc' },
            miniflare: {
                // The pinned vector's secret from docs/protocol.md; the real one never leaves the deployed Worker.
                bindings: { LOOM_TOKEN_SECRET: 'loom-test-secret' },
            },
        }),
    ],
    // loom-pipeline's tests run against its own Worker, in vitest.pipeline.config.ts.
    test: { exclude: [...configDefaults.exclude, 'test/pipeline/**'] },
});
