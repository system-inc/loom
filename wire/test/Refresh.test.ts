import { env } from 'cloudflare:workers';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { blobKey, FreshForMilliseconds } from '../source/Blobs';
import { blobPath, call, freshRun, randomBytes, sha256Hex, token } from './Helpers';

// Both buckets delete what they hold 7 days after its upload, so a held object written or asked for again past
// FreshFor is written again onto itself in R2, its own bytes, which starts its 7 days over; one within FreshFor is left
// as it is. R2 stamps an upload with its own clock, so these move the Worker's clock (Date) past FreshFor instead,
// which ages everything already held.
function age(milliseconds: number): void {
    vi.useFakeTimers({ toFake: ['Date'] });
    vi.setSystemTime(Date.now() + milliseconds);
}

afterEach(function () {
    vi.useRealTimers();
});

const stale = FreshForMilliseconds + 60 * 60 * 1000;

// A write moves an object's upload time on: R2's clock is real, and the Worker's has been moved ahead of it, so a
// rewrite is anything uploaded after the original.
function hex(buffer: ArrayBuffer | undefined): string {
    return [...new Uint8Array(buffer ?? new ArrayBuffer(0))]
        .map(function (byte) {
            return byte.toString(16).padStart(2, '0');
        })
        .join('');
}

async function uploaded(store: R2Bucket, key: string): Promise<number> {
    return (await store.head(key))?.uploaded.getTime() ?? 0;
}

describe('a held blob written again', function () {
    it('is left as it is while fresh, and refreshed in R2, its own bytes, once stale', async function () {
        const run = freshRun();
        const body = randomBytes(3 * 1024 * 1024 + 5);
        const sha256 = await sha256Hex(body);
        expect((await call(blobPath(run, sha256), { method: 'PUT', bearer: await token(run, 'runner'), body: body })).status).toBe(201);
        const first = await env.Store.head(blobKey(sha256));
        const again = await call(blobPath(run, sha256), { method: 'PUT', bearer: await token(run, 'runner'), body: body });
        expect(await again.json()).toMatchObject({ stored: false, refreshed: false });
        expect(await uploaded(env.Store, blobKey(sha256))).toBe(first?.uploaded.getTime());

        age(stale);
        const refreshed = await call(blobPath(run, sha256), { method: 'PUT', bearer: await token(run, 'coordinator'), body: body });
        expect(refreshed.status).toBe(200);
        expect(await refreshed.json()).toMatchObject({ sha256: sha256, bytes: body.length, stored: false, refreshed: true });
        const after = await env.Store.get(blobKey(sha256));
        expect(after?.uploaded.getTime()).toBeGreaterThan(first?.uploaded.getTime() ?? 0);
        // R2 checked the rewrite against the name, and keeps the checksum it checked.
        expect(hex(after?.checksums.sha256)).toBe(sha256);
        expect(new Uint8Array((await after?.arrayBuffer()) ?? new ArrayBuffer(0))).toEqual(body);
    }, 30000);

    it('is refreshed in the public store too, and a HEAD that a writer skips a PUT on refreshes it', async function () {
        const body = randomBytes(1024);
        const sha256 = await sha256Hex(body);
        const path = `/public/blobs/${sha256}`;
        expect((await call(path, { method: 'PUT', bearer: await token(freshRun(), 'coordinator'), body: body })).status).toBe(201);
        const first = await uploaded(env.PublicStore, blobKey(sha256));
        expect((await call(path, { method: 'HEAD', bearer: await token(freshRun(), 'coordinator') })).status).toBe(200);
        expect(await uploaded(env.PublicStore, blobKey(sha256))).toBe(first);

        age(stale);
        const head = await call(path, { method: 'HEAD', bearer: await token('workshop', 'publish') });
        expect(head.status).toBe(200);
        expect(head.headers.get('Content-Length')).toBe('1024');
        const refreshed = await uploaded(env.PublicStore, blobKey(sha256));
        expect(refreshed).toBeGreaterThan(first);
        // Refreshed just now, so back on R2's clock a PUT is left as it is.
        vi.useRealTimers();
        const put = await call(path, { method: 'PUT', bearer: await token('workshop', 'publish'), body: body });
        expect(await put.json()).toMatchObject({ stored: false, refreshed: false });
        expect(await uploaded(env.PublicStore, blobKey(sha256))).toBe(refreshed);
    });

    it("refreshes a run's input on the coordinator's HEAD", async function () {
        const run = freshRun();
        const body = randomBytes(64);
        const sha256 = await sha256Hex(body);
        expect((await call(blobPath(run, sha256), { method: 'PUT', bearer: await token(run, 'coordinator'), body: body })).status).toBe(201);
        const first = await uploaded(env.Store, blobKey(sha256));
        age(stale);
        expect((await call(blobPath(run, sha256), { method: 'HEAD', bearer: await token(run, 'coordinator') })).status).toBe(200);
        expect(await uploaded(env.Store, blobKey(sha256))).toBeGreaterThan(first);
    });

    it('replaces a held object that does not hash to its name with the verified body', async function () {
        const body = randomBytes(512);
        const sha256 = await sha256Hex(body);
        await env.PublicStore.put(blobKey(sha256), randomBytes(512));
        age(stale);
        const put = await call(`/public/blobs/${sha256}`, { method: 'PUT', bearer: await token('workshop', 'publish'), body: body });
        expect(put.status).toBe(201);
        expect(new Uint8Array((await (await env.PublicStore.get(blobKey(sha256)))?.arrayBuffer()) ?? new ArrayBuffer(0))).toEqual(body);
    });
});

describe('a held ref written again', function () {
    it('is refreshed once stale, its blob first, and never changed', async function () {
        const body = randomBytes(256);
        const sha256 = await sha256Hex(body);
        const name = await sha256Hex(randomBytes(8));
        const ref = `/public/refs/build/${name}`;
        const publisher = await token('workshop', 'publish');
        expect((await call(`/public/blobs/${sha256}`, { method: 'PUT', bearer: publisher, body: body })).status).toBe(201);
        expect((await call(ref, { method: 'PUT', bearer: publisher, body: sha256 })).status).toBe(201);
        const firstRef = await uploaded(env.PublicStore, `refs/build/${name}`);
        const firstBlob = await uploaded(env.PublicStore, blobKey(sha256));
        const fresh = await call(ref, { method: 'PUT', bearer: publisher, body: sha256 });
        expect(await fresh.json()).toMatchObject({ created: false, refreshed: false });
        expect(await uploaded(env.PublicStore, `refs/build/${name}`)).toBe(firstRef);

        age(stale);
        const later = await token('workshop', 'publish');
        const refreshed = await call(ref, { method: 'PUT', bearer: later, body: sha256 });
        expect(refreshed.status).toBe(200);
        expect(await refreshed.json()).toMatchObject({ created: false, refreshed: true });
        expect(await uploaded(env.PublicStore, `refs/build/${name}`)).toBeGreaterThan(firstRef);
        expect(await uploaded(env.PublicStore, blobKey(sha256))).toBeGreaterThan(firstBlob);
        expect(await (await env.PublicStore.get(`refs/build/${name}`))?.text()).toBe(sha256);
        // A stale ref asked to change is still refused, and left as it is.
        const other = await sha256Hex(randomBytes(16));
        expect((await call(ref, { method: 'PUT', bearer: later, body: other })).status).toBe(409);
    });
});

describe('a cache entry', function () {
    it('refreshes every stale blob it names before it is written', async function () {
        const run = freshRun();
        const output = randomBytes(40);
        const events = randomBytes(30);
        const coordinator = await token(run, 'coordinator');
        for (const body of [output, events]) {
            expect((await call(blobPath(run, await sha256Hex(body)), { method: 'PUT', bearer: coordinator, body: body })).status).toBe(201);
        }
        const first = await uploaded(env.Store, blobKey(await sha256Hex(output)));
        age(stale);
        const key = await sha256Hex(randomBytes(32));
        const entry = {
            key: key,
            run: run,
            unit: 'tests[shard=0]',
            machine: 'box',
            runnerVersion: 'v0-dev',
            wallSeconds: 1,
            outputs: [{ path: 'binaries/x.test', sha256: await sha256Hex(output), bytes: 40 }],
            events: await sha256Hex(events),
        };
        const put = await call(`/cache/${key}`, { method: 'PUT', bearer: await token(run, 'coordinator'), body: JSON.stringify(entry) });
        expect(put.status).toBe(201);
        expect(await uploaded(env.Store, blobKey(await sha256Hex(output)))).toBeGreaterThan(first);
        expect(await uploaded(env.Store, blobKey(await sha256Hex(events)))).toBeGreaterThan(first);
    });
});
