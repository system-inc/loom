import { env } from 'cloudflare:workers';
import { describe, expect, it } from 'vitest';
import { MaximumCacheEntryBytes } from '../source/Cache';
import { call, freshRun, randomBytes, sha256Hex, token, upload } from './Helpers';

function cachePath(key: string): string {
    return `/cache/${key}`;
}

// A key no other test uses, so write-once in one test can't meet another's entry.
async function freshKey(): Promise<string> {
    return sha256Hex(randomBytes(32));
}

// An output and an event log in the store, and an entry that names them, as protocol.CacheEntry marshals it.
async function storedEntry(key: string): Promise<Record<string, unknown>> {
    const run = freshRun();
    const coordinator = await token(run, 'coordinator');
    const output = await upload(run, coordinator, randomBytes(40));
    const eventLog = await upload(run, coordinator, randomBytes(30));
    return {
        key: key,
        run: run,
        unit: 'tests[shard=0]',
        machine: 'box',
        runnerVersion: 'v0-dev',
        wallSeconds: 1.25,
        outputs: [{ path: 'binaries/x.test', sha256: output.sha256, bytes: 40 }],
        events: eventLog.sha256,
    };
}

async function putEntry(key: string, bearer: string, body: unknown): Promise<Response> {
    return call(cachePath(key), { method: 'PUT', bearer: bearer, body: typeof body === 'string' ? body : JSON.stringify(body) });
}

describe('the cache', function () {
    it('writes an entry once and serves it back', async function () {
        const coordinator = await token(freshRun(), 'coordinator');
        const key = await freshKey();
        const entry = await storedEntry(key);
        expect((await call(cachePath(key), { bearer: coordinator })).status).toBe(404);
        const written = await putEntry(key, coordinator, entry);
        expect(written.status, await written.clone().text()).toBe(201);
        const read = await call(cachePath(key), { bearer: coordinator });
        expect(read.status).toBe(200);
        expect(await read.json()).toEqual(entry);
        expect(await (await env.Store.get(`cache/${key}`))?.json()).toEqual(entry);

        // The same entry again, or a different one under the same key, leaves the first as it is.
        expect((await putEntry(key, coordinator, entry)).status).toBe(200);
        const other = { ...(await storedEntry(key)), machine: 'another-box' };
        expect((await putEntry(key, coordinator, other)).status).toBe(200);
        expect(await (await call(cachePath(key), { bearer: coordinator })).json()).toEqual(entry);
    });

    it('takes an entry with no outputs', async function () {
        const key = await freshKey();
        const entry = { ...(await storedEntry(key)), outputs: [] };
        expect((await putEntry(key, await token(freshRun(), 'coordinator'), entry)).status).toBe(201);
    });

    it('takes any run\'s coordinator token, and nothing else', async function () {
        const key = await freshKey();
        const entry = await storedEntry(key);
        const run = freshRun();
        expect((await putEntry(key, await token(run, 'runner'), entry)).status).toBe(403);
        expect((await putEntry(key, await token(run, 'viewer'), entry)).status).toBe(403);
        expect((await call(cachePath(key), { bearer: await token(run, 'runner') })).status).toBe(403);
        expect((await call(cachePath(key), { bearer: await token(run, 'viewer') })).status).toBe(403);
        expect((await call(cachePath(key))).status).toBe(401);
        const queried = `${cachePath(key)}?token=${encodeURIComponent(await token(run, 'coordinator'))}`;
        expect((await call(queried)).status).toBe(401);
        expect((await call(cachePath(key.toUpperCase()), { bearer: await token(run, 'coordinator') })).status).toBe(400);
        expect((await call(cachePath('abc'), { bearer: await token(run, 'coordinator') })).status).toBe(400);
        expect((await call(cachePath(key), { method: 'POST', bearer: await token(run, 'coordinator'), body: '{}' })).status).toBe(405);
        // None of those wrote anything; another run's coordinator writes it.
        expect((await putEntry(key, await token(freshRun(), 'coordinator'), entry)).status).toBe(201);
    });

    it("refuses an entry whose key isn't the path's", async function () {
        const coordinator = await token(freshRun(), 'coordinator');
        const key = await freshKey();
        const elsewhere = await freshKey();
        const response = await putEntry(key, coordinator, await storedEntry(elsewhere));
        expect(response.status).toBe(400);
        expect(((await response.json()) as { error: string }).error).toContain(elsewhere);
        expect(await env.Store.head(`cache/${key}`)).toBeNull();
        expect(await env.Store.head(`cache/${elsewhere}`)).toBeNull();
    });

    it("refuses an entry whose outputs or event log aren't in the store", async function () {
        const coordinator = await token(freshRun(), 'coordinator');
        const key = await freshKey();
        const entry = await storedEntry(key);
        const absent = await sha256Hex(randomBytes(32));
        const missingOutput = {
            ...entry,
            outputs: [...(entry.outputs as unknown[]), { path: 'logs/run.txt', sha256: absent, bytes: 9 }],
        };
        const refused = await putEntry(key, coordinator, missingOutput);
        expect(refused.status).toBe(409);
        expect(await refused.json()).toMatchObject({ missing: [absent] });
        expect((await putEntry(key, coordinator, { ...entry, events: absent })).status).toBe(409);
        expect(await env.Store.head(`cache/${key}`)).toBeNull();
        expect((await putEntry(key, coordinator, entry)).status).toBe(201);
    });

    it('refuses anything but protocol.CacheEntry', async function () {
        const coordinator = await token(freshRun(), 'coordinator');
        const key = await freshKey();
        const entry = await storedEntry(key);
        const output = (entry.outputs as Record<string, unknown>[])[0] as Record<string, unknown>;
        const without = function (name: string): Record<string, unknown> {
            const copy = { ...entry };
            delete copy[name];
            return copy;
        };
        const malformed: [string, unknown][] = [
            ['not JSON', 'nope'],
            ['a list', '[]'],
            ['null', 'null'],
            ['no key', without('key')],
            ['no run', without('run')],
            ['no unit', without('unit')],
            ['no machine', without('machine')],
            ['no runnerVersion', without('runnerVersion')],
            ['no wallSeconds', without('wallSeconds')],
            ['no outputs', without('outputs')],
            ['no events', without('events')],
            ['an extra field', { ...entry, status: 'passed' }],
            ['a capitalized field', { ...without('key'), Key: key }],
            ['an uppercase key', { ...entry, key: key.toUpperCase() }],
            ['a run id the wire refuses', { ...entry, run: 'bad/run' }],
            ['an empty run', { ...entry, run: '' }],
            ['an empty unit', { ...entry, unit: '' }],
            ['a unit past 256 bytes', { ...entry, unit: 'é'.repeat(129) }],
            ['a numeric machine', { ...entry, machine: 3 }],
            ['a null runnerVersion', { ...entry, runnerVersion: null }],
            ['a string wallSeconds', { ...entry, wallSeconds: '1.25' }],
            ['null outputs', { ...entry, outputs: null }],
            ['an output with an extra field', { ...entry, outputs: [{ ...output, mode: '0755' }] }],
            ['an output with no bytes', { ...entry, outputs: [{ path: output.path, sha256: output.sha256 }] }],
            ['an output with an empty path', { ...entry, outputs: [{ ...output, path: '' }] }],
            ['an output with a short sha256', { ...entry, outputs: [{ ...output, sha256: 'abc' }] }],
            ['an output with negative bytes', { ...entry, outputs: [{ ...output, bytes: -1 }] }],
            ['an output with fractional bytes', { ...entry, outputs: [{ ...output, bytes: 1.5 }] }],
            ['an output that is a string', { ...entry, outputs: ['binaries/x.test'] }],
            ['events that are not a sha256', { ...entry, events: 'log' }],
        ];
        for (const [name, body] of malformed) {
            const response = await putEntry(key, coordinator, body);
            expect(response.status, name).toBe(400);
        }
        const oversized = JSON.stringify({ ...entry, machine: 'x'.repeat(MaximumCacheEntryBytes) });
        expect((await putEntry(key, coordinator, oversized)).status).toBe(413);
        expect(await env.Store.head(`cache/${key}`)).toBeNull();
        // A unit id of 256 bytes is the longest taken.
        expect((await putEntry(key, coordinator, { ...entry, unit: 'é'.repeat(128) })).status).toBe(201);
    });
});
