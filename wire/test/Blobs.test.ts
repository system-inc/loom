import { env } from 'cloudflare:workers';
import { describe, expect, it } from 'vitest';
import { blobKey, MaximumBlobBytes } from '../source/Blobs';
import { call, freshRun, token } from './Helpers';

async function sha256Hex(bytes: Uint8Array): Promise<string> {
    const digest = await crypto.subtle.digest('SHA-256', bytes);
    return Array.from(new Uint8Array(digest), function (byte) {
        return byte.toString(16).padStart(2, '0');
    }).join('');
}

function randomBytes(length: number): Uint8Array {
    const bytes = new Uint8Array(length);
    for (let offset = 0; offset < length; offset += 65536) {
        crypto.getRandomValues(bytes.subarray(offset, Math.min(length, offset + 65536)));
    }
    return bytes;
}

describe('blobs', function () {
    it('stores a body whose hash matches and serves it back', async function () {
        const runner = await token(freshRun(), 'runner');
        const body = randomBytes(3 * 1024 * 1024 + 17);
        const sha256 = await sha256Hex(body);
        const put = await call(`/blobs/${sha256}`, { method: 'PUT', bearer: runner, body: body });
        expect(put.status).toBe(201);
        expect(await put.json()).toMatchObject({ sha256: sha256, bytes: body.length, stored: true });
        const get = await call(`/blobs/${sha256}`, { bearer: runner });
        expect(get.status).toBe(200);
        expect(new Uint8Array(await get.arrayBuffer())).toEqual(body);
    });

    it('refuses a body that hashes to something else, and stores nothing', async function () {
        const coordinator = await token(freshRun(), 'coordinator');
        const body = randomBytes(1024);
        const claimed = await sha256Hex(randomBytes(32));
        const put = await call(`/blobs/${claimed}`, { method: 'PUT', bearer: coordinator, body: body });
        expect(put.status).toBe(400);
        expect(await env.Store.head(blobKey(claimed))).toBeNull();
        expect((await call(`/blobs/${claimed}`, { bearer: coordinator })).status).toBe(404);
    });

    it('leaves an existing blob as it is', async function () {
        const runner = await token(freshRun(), 'runner');
        const body = randomBytes(2048);
        const sha256 = await sha256Hex(body);
        expect((await call(`/blobs/${sha256}`, { method: 'PUT', bearer: runner, body: body })).status).toBe(201);
        const before = await env.Store.head(blobKey(sha256));
        const again = await call(`/blobs/${sha256}`, { method: 'PUT', bearer: runner, body: body });
        expect(again.status).toBe(200);
        expect(await again.json()).toMatchObject({ stored: false });
        const after = await env.Store.head(blobKey(sha256));
        expect(after?.uploaded.getTime()).toBe(before?.uploaded.getTime());
        expect(after?.etag).toBe(before?.etag);
    });

    it('stores the empty blob', async function () {
        const runner = await token(freshRun(), 'runner');
        const sha256 = await sha256Hex(new Uint8Array(0));
        const put = await call(`/blobs/${sha256}`, { method: 'PUT', bearer: runner, body: '' });
        expect(put.status).toBe(201);
        expect((await call(`/blobs/${sha256}`, { bearer: runner })).status).toBe(200);
    });

    it('refuses viewers, bad addresses and oversized bodies', async function () {
        const run = freshRun();
        const sha256 = await sha256Hex(new Uint8Array([1]));
        expect((await call(`/blobs/${sha256}`, { bearer: await token(run, 'viewer') })).status).toBe(403);
        expect((await call(`/blobs/${sha256}`)).status).toBe(401);
        expect((await call(`/blobs/${sha256.toUpperCase()}`, { bearer: await token(run, 'runner') })).status).toBe(400);
        const oversized = await call(`/blobs/${sha256}`, {
            method: 'PUT',
            bearer: await token(run, 'runner'),
            headers: { 'Content-Length': String(MaximumBlobBytes + 1) },
            body: new Uint8Array([1]),
        });
        expect(oversized.status).toBe(413);
    });
});
