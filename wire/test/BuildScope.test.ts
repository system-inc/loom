// loom-runs stays frozen until cutover, and a build token (loom's action store) reaches nothing on it.

import { exports } from 'cloudflare:workers';
import { describe, expect, it } from 'vitest';
import { call, freshRun, Origin, randomBytes, sha256Hex, token } from './Helpers';

describe('a build token on loom-runs', function () {
    it('reaches no public blob or ref, no run, no cache, and no /actions', async function () {
        const workshop = await token('workshop', 'build');
        const key = await sha256Hex(randomBytes(32));
        const manifest = await sha256Hex(randomBytes(32));
        const run = freshRun();
        const blob = randomBytes(64);
        const blobSha256 = await sha256Hex(blob);
        expect((await call(`/public/blobs/${blobSha256}`, { method: 'PUT', bearer: workshop, body: blob })).status).toBe(403);
        expect((await call(`/public/refs/build/${key}`, { method: 'PUT', bearer: workshop, body: manifest })).status).toBe(403);
        expect((await call(`/public/refs/action/${key}`, { method: 'PUT', bearer: workshop, body: manifest })).status).toBe(403);
        expect((await call(`/runs/${run}/blobs/${blobSha256}`, { bearer: workshop })).status).toBe(403);
        expect((await call(`/runs/${run}/plan`, { method: 'POST', bearer: workshop, body: '{}' })).status).toBe(403);
        expect((await call(`/cache/${key}`, { bearer: workshop })).status).toBe(403);
        // Nor does the live wire route /actions at all.
        expect((await (exports as unknown as { default: Fetcher }).default.fetch(`${Origin}/actions/${key}`, {
            method: 'PUT',
            headers: { Authorization: `Bearer ${workshop}` },
            body: manifest,
        })).status).toBe(404);
    });
});
