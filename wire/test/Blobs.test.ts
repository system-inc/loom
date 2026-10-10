import { env } from 'cloudflare:workers';
import { describe, expect, it } from 'vitest';
import { blobKey, MaximumBlobBytes } from '../source/Blobs';
import {
    blobPath,
    call,
    freshRun,
    postPlan,
    postVerdict,
    randomBytes,
    sha256Hex,
    token,
    upload,
    verdictBody,
} from './Helpers';

describe('blobs', function () {
    it('stores a body whose hash matches and serves it back', async function () {
        const run = freshRun();
        const runner = await token(run, 'runner');
        const body = randomBytes(3 * 1024 * 1024 + 17);
        const sha256 = await sha256Hex(body);
        const put = await call(blobPath(run, sha256), { method: 'PUT', bearer: runner, body: body });
        expect(put.status).toBe(201);
        expect(await put.json()).toMatchObject({ sha256: sha256, bytes: body.length, stored: true });
        const get = await call(blobPath(run, sha256), { bearer: runner });
        expect(get.status).toBe(200);
        expect(new Uint8Array(await get.arrayBuffer())).toEqual(body);
    });

    it('refuses a body that hashes to something else, and stores and records nothing', async function () {
        const run = freshRun();
        const coordinator = await token(run, 'coordinator');
        const body = randomBytes(1024);
        const claimed = await sha256Hex(randomBytes(32));
        const put = await call(blobPath(run, claimed), { method: 'PUT', bearer: coordinator, body: body });
        expect(put.status).toBe(400);
        expect(await env.Store.head(blobKey(claimed))).toBeNull();
        expect((await call(blobPath(run, claimed), { bearer: coordinator })).status).toBe(404);
        // A refused PUT isn't the run's upload, so a runner may not read the hash.
        expect((await call(blobPath(run, claimed), { bearer: await token(run, 'runner') })).status).toBe(403);
    });

    it('leaves an existing blob as it is', async function () {
        const run = freshRun();
        const runner = await token(run, 'runner');
        const body = randomBytes(2048);
        const sha256 = await sha256Hex(body);
        expect((await call(blobPath(run, sha256), { method: 'PUT', bearer: runner, body: body })).status).toBe(201);
        const before = await env.Store.head(blobKey(sha256));
        const again = await call(blobPath(run, sha256), { method: 'PUT', bearer: runner, body: body });
        expect(again.status).toBe(200);
        expect(await again.json()).toMatchObject({ stored: false });
        const after = await env.Store.head(blobKey(sha256));
        expect(after?.uploaded.getTime()).toBe(before?.uploaded.getTime());
        expect(after?.etag).toBe(before?.etag);
    });

    it('stores the empty blob', async function () {
        const run = freshRun();
        const runner = await token(run, 'runner');
        const sha256 = await sha256Hex(new Uint8Array(0));
        const put = await call(blobPath(run, sha256), { method: 'PUT', bearer: runner, body: '' });
        expect([200, 201]).toContain(put.status); // another test may have stored it first
        expect((await call(blobPath(run, sha256), { bearer: runner })).status).toBe(200);
    });

    it('refuses bad addresses, the old route, other methods and oversized bodies', async function () {
        const run = freshRun();
        const runner = await token(run, 'runner');
        const sha256 = await sha256Hex(new Uint8Array([1]));
        expect((await call(blobPath(run, sha256))).status).toBe(401);
        expect((await call(blobPath(run, sha256.toUpperCase()), { bearer: runner })).status).toBe(400);
        expect((await call(blobPath('-bad', sha256), { bearer: runner })).status).toBe(400);
        expect((await call(`/runs/${run}/blobs`, { bearer: runner })).status).toBe(404);
        // v0.1's unscoped route is gone, not forwarded.
        expect((await call(`/blobs/${sha256}`, { bearer: runner })).status).toBe(404);
        expect((await call(blobPath(run, sha256), { method: 'DELETE', bearer: runner })).status).toBe(405);
        const oversized = await call(blobPath(run, sha256), {
            method: 'PUT',
            bearer: runner,
            headers: { 'Content-Length': String(MaximumBlobBytes + 1) },
            body: new Uint8Array([1]),
        });
        expect(oversized.status).toBe(413);
    });
});

// Three blobs already in the store through another run, so this run has neither planned nor uploaded them
// unless a test says so.
async function storedElsewhere(count: number): Promise<string[]> {
    const elsewhere = freshRun();
    const coordinator = await token(elsewhere, 'coordinator');
    const hashes: string[] = [];
    for (let index = 0; index < count; index++) {
        const stored = await upload(elsewhere, coordinator, randomBytes(100 + index));
        expect(stored.status).toBe(201);
        hashes.push(stored.sha256);
    }
    return hashes;
}

describe('who reaches a blob', function () {
    it('reads by the Store table: GET and HEAD for each scope', async function () {
        const run = freshRun();
        const other = freshRun();
        const coordinator = await token(run, 'coordinator');
        const runner = await token(run, 'runner');
        const viewer = await token(run, 'viewer');
        const [input, foreign] = (await storedElsewhere(2)) as [string, string];
        const absentInput = await sha256Hex(randomBytes(32)); // planned, never stored
        const absent = await sha256Hex(randomBytes(32)); // neither
        expect((await postPlan(run, coordinator, ['a'], [input, absentInput])).status).toBe(201);
        const output = await upload(run, runner, randomBytes(321));
        expect(output.status).toBe(201);

        const reads: [string, string, string, number][] = [
            ['runner, a planned input', runner, input, 200],
            ["runner, its run's upload", runner, output.sha256, 200],
            ['runner, a hash neither planned nor uploaded', runner, foreign, 403],
            ['runner, a hash neither planned nor uploaded, absent', runner, absent, 403],
            ['runner, a planned input not in the store', runner, absentInput, 404],
            ['coordinator, any hash', coordinator, foreign, 200],
            ['coordinator, a planned input', coordinator, input, 200],
            ['coordinator, a hash not in the store', coordinator, absent, 404],
            ["viewer, its run's upload", viewer, output.sha256, 200],
            ['viewer, a planned input it never uploaded', viewer, input, 403],
            ['viewer, a hash of another run', viewer, foreign, 403],
            ["another run's runner", await token(other, 'runner'), output.sha256, 403],
            ["another run's coordinator", await token(other, 'coordinator'), foreign, 403],
            ["another run's viewer", await token(other, 'viewer'), output.sha256, 403],
        ];
        for (const [who, bearer, sha256, status] of reads) {
            const get = await call(blobPath(run, sha256), { bearer: bearer });
            expect(get.status, `GET ${who}`).toBe(status);
            await get.body?.cancel();
            const head = await call(blobPath(run, sha256), { method: 'HEAD', bearer: bearer });
            expect(head.status, `HEAD ${who}`).toBe(status);
            expect(await head.text(), `HEAD ${who} has no body`).toBe('');
        }
        const head = await call(blobPath(run, output.sha256), { method: 'HEAD', bearer: runner });
        expect(head.headers.get('Content-Length')).toBe('321');
    });

    it('takes a viewer token as ?token=, and no other scope that way', async function () {
        const run = freshRun();
        const runner = await token(run, 'runner');
        const output = await upload(run, runner, randomBytes(64));
        const query = function (bearer: string) {
            return `${blobPath(run, output.sha256)}?token=${encodeURIComponent(bearer)}`;
        };
        const viewed = await call(query(await token(run, 'viewer')));
        expect(viewed.status).toBe(200);
        expect((await viewed.arrayBuffer()).byteLength).toBe(64);
        expect((await call(query(await token(run, 'viewer')), { method: 'HEAD' })).status).toBe(200);
        expect((await call(query(runner))).status).toBe(401);
        expect((await call(query(await token(run, 'coordinator')))).status).toBe(401);
        expect((await call(query(await token(freshRun(), 'viewer')))).status).toBe(403);
        // A viewer can't upload, whichever way its token comes.
        const bytes = randomBytes(8);
        const sha256 = await sha256Hex(bytes);
        const viewerPut = `${blobPath(run, sha256)}?token=${encodeURIComponent(await token(run, 'viewer'))}`;
        expect((await call(viewerPut, { method: 'PUT', body: bytes })).status).toBe(403);
        expect((await call(blobPath(run, sha256), { method: 'PUT', bearer: await token(run, 'viewer'), body: bytes })).status).toBe(403);
        expect(await env.Store.head(blobKey(sha256))).toBeNull();
    });

    it('writes for a runner or coordinator of the run, and records even a blob already held', async function () {
        const run = freshRun();
        const runner = await token(run, 'runner');
        const viewer = await token(run, 'viewer');
        const [held] = (await storedElsewhere(1)) as [string];
        expect((await call(blobPath(run, held), { bearer: runner })).status).toBe(403);
        const bytes = await ((await env.Store.get(blobKey(held))) as R2ObjectBody).arrayBuffer();
        // Not the right run: refused before a byte is read.
        const stranger = await call(blobPath(run, held), { method: 'PUT', bearer: await token(freshRun(), 'runner'), body: bytes });
        expect(stranger.status).toBe(403);
        expect((await call(blobPath(run, held), { bearer: viewer })).status).toBe(403);
        // Already in R2, so 200 and left as is; it still counts as this run's upload.
        expect((await upload(run, runner, new Uint8Array(bytes))).status).toBe(200);
        expect((await call(blobPath(run, held), { bearer: runner })).status).toBe(200);
        expect((await call(blobPath(run, held), { bearer: viewer })).status).toBe(200);
        expect((await upload(run, await token(run, 'coordinator'), randomBytes(77))).status).toBe(201);
    });

    it('records nothing on a HEAD', async function () {
        const run = freshRun();
        const coordinator = await token(run, 'coordinator');
        const runner = await token(run, 'runner');
        const viewer = await token(run, 'viewer');
        const [held] = (await storedElsewhere(1)) as [string];
        const head = await call(blobPath(run, held), { method: 'HEAD', bearer: coordinator });
        expect(head.status).toBe(200);
        expect(head.headers.get('Content-Length')).toBe('100');
        // The coordinator only asked; the hash is still not this run's.
        expect((await call(blobPath(run, held), { bearer: runner })).status).toBe(403);
        expect((await call(blobPath(run, held), { method: 'HEAD', bearer: runner })).status).toBe(403);
        expect((await call(blobPath(run, held), { bearer: viewer })).status).toBe(403);
        await postPlan(run, coordinator, ['a']);
        expect((await postVerdict(run, coordinator, verdictBody('void', [], ['a never finished']))).status).toBe(201);
        expect(await (await env.Store.get(`runs/${run}/blobs.jsonl`))?.text()).toBe('');
    });

    it("refuses a runner's PUT once the run has its verdict, but not its reads or the coordinator", async function () {
        const run = freshRun();
        const coordinator = await token(run, 'coordinator');
        const runner = await token(run, 'runner');
        const before = await upload(run, runner, randomBytes(50));
        await postPlan(run, coordinator, ['a']);
        expect((await postVerdict(run, coordinator, verdictBody('void', [], ['a never finished']))).status).toBe(201);
        const late = randomBytes(60);
        const lateHash = await sha256Hex(late);
        expect((await call(blobPath(run, lateHash), { method: 'PUT', bearer: runner, body: late })).status).toBe(409);
        expect(await env.Store.head(blobKey(lateHash))).toBeNull();
        // Even a blob already held: the run takes no more runner uploads.
        expect((await call(blobPath(run, before.sha256), { method: 'PUT', bearer: runner, body: randomBytes(50) })).status).toBe(409);
        expect((await call(blobPath(run, before.sha256), { bearer: runner })).status).toBe(200);
        expect((await upload(run, coordinator, late)).status).toBe(201);
    });

    it('lists the run\'s uploads in runs/<run>/blobs.jsonl when the verdict archives it', async function () {
        const run = freshRun();
        const coordinator = await token(run, 'coordinator');
        const runner = await token(run, 'runner');
        const [held] = (await storedElsewhere(1)) as [string];
        const heldBytes = await env.Store.get(blobKey(held));
        const input = await upload(run, coordinator, randomBytes(10));
        const outputBytes = randomBytes(20);
        const output = await upload(run, runner, outputBytes);
        expect((await upload(run, runner, new Uint8Array(await (heldBytes as R2ObjectBody).arrayBuffer()))).status).toBe(200);
        // A second PUT of a hash the run already uploaded adds no line, whoever sends it; nor does a HEAD.
        expect((await upload(run, coordinator, outputBytes)).status).toBe(200);
        expect((await call(blobPath(run, output.sha256), { method: 'HEAD', bearer: runner })).status).toBe(200);
        await postPlan(run, coordinator, ['a'], [input.sha256]);
        expect((await postVerdict(run, coordinator, verdictBody('void', [], ['a never finished']))).status).toBe(201);
        const archived = await env.Store.get(`runs/${run}/blobs.jsonl`);
        const lines = ((await archived?.text()) ?? '').trim().split('\n').map(function (line) {
            return JSON.parse(line) as unknown;
        });
        expect(lines).toEqual([
            { sha256: input.sha256, bytes: 10, scope: 'coordinator' },
            { sha256: output.sha256, bytes: 20, scope: 'runner' },
            { sha256: held, bytes: 100, scope: 'runner' },
        ]);
    });
});

describe('the public store', function () {
    it('takes writes from a coordinator only, checks the hash, and serves no reads through the Worker', async function () {
        const run = freshRun();
        const coordinator = await token(run, 'coordinator');
        const body = randomBytes(2048);
        const sha256 = await sha256Hex(body);
        const path = `/public/blobs/${sha256}`;
        expect((await call(path, { method: 'PUT', bearer: await token(run, 'runner'), body: body })).status).toBe(403);
        expect((await call(path, { method: 'PUT', bearer: await token(run, 'viewer'), body: body })).status).toBe(403);
        expect((await call(`/public/blobs/${await sha256Hex(randomBytes(8))}`, { method: 'PUT', bearer: coordinator, body: body })).status).toBe(400);
        expect((await call(path, { method: 'HEAD', bearer: coordinator })).status).toBe(404);
        expect((await call(path, { method: 'PUT', bearer: coordinator, body: body })).status).toBe(201);
        expect((await call(path, { method: 'HEAD', bearer: coordinator })).status).toBe(200);
        expect((await call(path, { bearer: coordinator })).status).toBe(405);
        expect(await env.PublicStore.head(`blobs/${sha256}`)).not.toBeNull();
        // loom-wire's blobs carry no Cache-Control of their own; only the action store marks its blobs immutable.
        expect((await env.PublicStore.head(`blobs/${sha256}`))?.httpMetadata?.cacheControl).toBeUndefined();
        // The private store never saw it.
        expect(await env.Store.head(`blobs/${sha256}`)).toBeNull();
    });
});

describe('public refs', function () {
    it('lets a publish token write blobs and refs, and nothing else', async function () {
        const publisher = await token('workshop', 'publish');
        const body = randomBytes(512);
        const sha256 = await sha256Hex(body);
        expect((await call(`/public/blobs/${sha256}`, { method: 'PUT', bearer: publisher, body: body })).status).toBe(201);
        const ref = `/public/refs/build/${await sha256Hex(randomBytes(8))}`;
        expect((await call(ref, { method: 'PUT', bearer: publisher, body: sha256 })).status).toBe(201);
        // A publish token reaches no run.
        const run = freshRun();
        expect((await call(`/runs/${run}/blobs/${sha256}`, { bearer: publisher })).status).toBe(403);
        expect((await call(`/runs/${run}/plan`, { method: 'POST', bearer: publisher, body: '{}' })).status).toBe(403);
    });

    it("lets a candidate's publish token write blobs and refs/build-candidate, never refs/build", async function () {
        const candidate = await token('codex-unit', 'publish-candidate');
        const body = randomBytes(512);
        const sha256 = await sha256Hex(body);
        expect((await call(`/public/blobs/${sha256}`, { method: 'PUT', bearer: candidate, body: body })).status).toBe(201);
        const name = await sha256Hex(randomBytes(8));
        expect((await call(`/public/refs/build/${name}`, { method: 'PUT', bearer: candidate, body: sha256 })).status).toBe(403);
        expect(await env.PublicStore.head(`refs/build/${name}`)).toBeNull();
        expect((await call(`/public/refs/build-candidate/${name}`, { method: 'PUT', bearer: candidate, body: sha256 })).status).toBe(201);
        expect((await call(`/public/refs/gocache-candidate/${name}`, { method: 'PUT', bearer: candidate, body: sha256 })).status).toBe(201);
        expect((await call(`/public/refs/gocache/${name}`, { method: 'PUT', bearer: candidate, body: sha256 })).status).toBe(403);
        // main's own gate's token writes either.
        const publisher = await token('home', 'publish');
        expect((await call(`/public/refs/build/${name}`, { method: 'PUT', bearer: publisher, body: sha256 })).status).toBe(201);
        expect((await call(`/runs/${freshRun()}/plan`, { method: 'POST', bearer: candidate, body: '{}' })).status).toBe(403);
    });

    it('stores a ref to a held blob, refuses a dangling or changed one, and serves no reads', async function () {
        const coordinator = await token(freshRun(), 'coordinator');
        const body = randomBytes(256);
        const sha256 = await sha256Hex(body);
        const other = await sha256Hex(randomBytes(256));
        const name = await sha256Hex(randomBytes(8));
        const ref = `/public/refs/build/${name}`;
        expect((await call(ref, { method: 'PUT', bearer: coordinator, body: sha256 })).status).toBe(409);
        expect(await env.PublicStore.head(`refs/build/${name}`)).toBeNull();
        expect((await call(`/public/blobs/${sha256}`, { method: 'PUT', bearer: coordinator, body: body })).status).toBe(201);
        expect((await call(ref, { method: 'PUT', bearer: coordinator, body: 'not a hash' })).status).toBe(400);
        expect((await call(ref, { method: 'PUT', bearer: await token(freshRun(), 'runner'), body: sha256 })).status).toBe(403);
        expect((await call(ref, { method: 'PUT', bearer: coordinator, body: sha256 })).status).toBe(201);
        expect((await (await env.PublicStore.get(`refs/build/${name}`))?.text())).toBe(sha256);
        expect((await call(ref, { method: 'PUT', bearer: coordinator, body: sha256 + '\n' })).status).toBe(200);
        expect((await call(ref, { method: 'PUT', bearer: coordinator, body: other })).status).toBe(409);
        expect((await (await env.PublicStore.get(`refs/build/${name}`))?.text())).toBe(sha256);
        expect((await call(ref, { bearer: coordinator })).status).toBe(405);
        expect((await call('/public/refs/Build/x', { method: 'PUT', bearer: coordinator, body: sha256 })).status).toBe(400);
        expect((await call('/public/refs/build/.hidden', { method: 'PUT', bearer: coordinator, body: sha256 })).status).toBe(400);
    });
});
