import { env, exports } from 'cloudflare:workers';
import { describe, expect, it } from 'vitest';
import { actionRef, canonicalManifest, checkManifest, handleAction, type ActionOutput } from '../../source/Actions';
import { freshRun, Origin, randomBytes, sha256Hex, TestSecret, token } from '../Helpers';

const environment = { PublicStore: env.PublicStore, LOOM_TOKEN_SECRET: TestSecret };

function action(path: string, init: RequestInit & { bearer?: string } = {}): Promise<Response | null> {
    const headers = new Headers(init.headers);
    if (init.bearer !== undefined) {
        headers.set('Authorization', `Bearer ${init.bearer}`);
    }
    // A request built here carries no Content-Length the way one through fetch does, and a blob PUT needs it.
    if (typeof init.body === 'string') {
        headers.set('Content-Length', String(new TextEncoder().encode(init.body).length));
    }
    else if (init.body instanceof Uint8Array) {
        headers.set('Content-Length', String(init.body.length));
    }
    return handleAction(new Request(Origin + path, { ...init, headers: headers }), environment);
}

async function answer(path: string, init: RequestInit & { bearer?: string } = {}): Promise<Response> {
    const response = await action(path, init);
    if (response === null) {
        throw new Error(`${path} isn't an action route`);
    }
    return response;
}

// Puts each output's bytes and the manifest through the action store, as Workshop would, and returns the
// manifest's sha256 for the ref.
async function putProduct(builder: string, key: string, files: Record<string, Uint8Array>): Promise<string> {
    const outputs: ActionOutput[] = [];
    for (const [path, bytes] of Object.entries(files)) {
        const sha256 = await sha256Hex(bytes);
        expect((await answer(`/actions/blobs/${sha256}`, { method: 'PUT', bearer: builder, body: bytes })).status).toBeLessThan(300);
        outputs.push({ path: path, sha256: sha256, bytes: bytes.length });
    }
    const manifest = new TextEncoder().encode(canonicalManifest({ key: key, outputs: outputs }));
    const manifestSha256 = await sha256Hex(manifest);
    expect((await answer(`/actions/blobs/${manifestSha256}`, { method: 'PUT', bearer: builder, body: manifest })).status).toBeLessThan(300);
    return manifestSha256;
}

async function freshKey(): Promise<string> {
    return sha256Hex(randomBytes(32));
}

describe('the action store', function () {
    it("writes an action's ref once, with its builder, to a manifest whose outputs are all held", async function () {
        const workshop = await token('workshop', 'build');
        const key = await freshKey();
        const manifest = await putProduct(workshop, key, { 'bin/oracle': randomBytes(4096), 'lib/runtime.a': randomBytes(512) });
        const put = await answer(`/actions/${key}`, { method: 'PUT', bearer: workshop, body: manifest });
        expect(put.status).toBe(201);
        expect(await put.json()).toMatchObject({ ref: actionRef(key), sha256: manifest, builder: 'workshop', created: true });
        const stored = await env.PublicStore.get(actionRef(key));
        expect(await stored?.text()).toBe(manifest);
        expect(stored?.customMetadata).toEqual({ builder: 'workshop' });
        // The same bytes again, from any builder, is a quiet no-op that names who built it first.
        const again = await answer(`/actions/${key}`, { method: 'PUT', bearer: await token('home', 'build'), body: manifest + '\n' });
        expect(again.status).toBe(200);
        expect(await again.json()).toMatchObject({ builder: 'workshop', created: false });
    });

    it('refuses a second write naming other bytes, naming both builders, and keeps the first', async function () {
        const key = await freshKey();
        const first = await putProduct(await token('workshop', 'build'), key, { out: randomBytes(1000) });
        const home = await token('home', 'build');
        const second = await putProduct(home, key, { out: randomBytes(1000) });
        expect((await answer(`/actions/${key}`, { method: 'PUT', bearer: await token('workshop', 'build'), body: first })).status).toBe(201);
        const conflict = await answer(`/actions/${key}`, { method: 'PUT', bearer: home, body: second });
        expect(conflict.status).toBe(409);
        const body = (await conflict.json()) as Record<string, string>;
        expect(body).toMatchObject({ held: first, heldBuilder: 'workshop', builder: 'home', sha256: second });
        expect(body.error).toContain('workshop built');
        expect(body.error).toContain('home built');
        expect(await (await env.PublicStore.get(actionRef(key)))?.text()).toBe(first);
    });

    it('lets two builders race one key: exactly one ref is written, and the loser hears whose it is', async function () {
        const key = await freshKey();
        const builders = await Promise.all(['workshop', 'home', 'cloud', 'server'].map(function (name) {
            return token(name, 'build');
        }));
        const manifests = await Promise.all(builders.map(function (builder) {
            return putProduct(builder, key, { out: randomBytes(64) });
        }));
        const answers = await Promise.all(builders.map(function (builder, index) {
            return answer(`/actions/${key}`, { method: 'PUT', bearer: builder, body: manifests[index] });
        }));
        const statuses = answers.map(function (response) {
            return response.status;
        });
        expect(statuses.filter(function (status) {
            return status === 201;
        })).toHaveLength(1);
        expect(statuses.filter(function (status) {
            return status === 409;
        })).toHaveLength(3);
        const winner = statuses.indexOf(201);
        expect(await (await env.PublicStore.get(actionRef(key)))?.text()).toBe(manifests[winner]);
    });

    it('takes writes from a build token only, and a build token reaches nothing else', async function () {
        const key = await freshKey();
        const workshop = await token('workshop', 'build');
        const manifest = await putProduct(workshop, key, { out: randomBytes(100) });
        const run = freshRun();
        for (const scope of ['runner', 'viewer', 'coordinator', 'publish', 'publish-candidate', 'pool', 'board', 'submit'] as const) {
            const other = await token(scope === 'board' ? 'board' : run, scope);
            expect((await answer(`/actions/${key}`, { method: 'PUT', bearer: other, body: manifest })).status, scope).toBe(403);
            expect((await answer(`/actions/blobs/${manifest}`, { method: 'HEAD', bearer: other })).status, scope).toBe(403);
        }
        expect((await answer(`/actions/${key}`, { method: 'PUT', body: manifest })).status).toBe(401);
        expect((await answer(`/actions/${key}`, { method: 'PUT', bearer: await token('workshop', 'build', -10), body: manifest })).status).toBe(401);
        expect(await env.PublicStore.head(actionRef(key))).toBeNull();
    });

    it('refuses a ref to a manifest that is missing, malformed, for another key, or names outputs not held', async function () {
        const workshop = await token('workshop', 'build');
        const key = await freshKey();
        const put = function (body: string): Promise<Response> {
            return answer(`/actions/${key}`, { method: 'PUT', bearer: workshop, body: body });
        };
        expect((await put('not a hash')).status).toBe(400);
        expect((await put(await freshKey())).status).toBe(409);
        const store = async function (text: string): Promise<string> {
            const bytes = new TextEncoder().encode(text);
            const sha256 = await sha256Hex(bytes);
            expect((await answer(`/actions/blobs/${sha256}`, { method: 'PUT', bearer: workshop, body: bytes })).status).toBeLessThan(300);
            return sha256;
        };
        const held = randomBytes(10);
        const heldOutput = { path: 'out', sha256: await sha256Hex(held), bytes: 10 };
        await answer(`/actions/blobs/${heldOutput.sha256}`, { method: 'PUT', bearer: workshop, body: held });
        const otherKey = await store(canonicalManifest({ key: await freshKey(), outputs: [heldOutput] }));
        const forOther = await put(otherKey);
        expect(forOther.status).toBe(400);
        expect(await forOther.json()).toMatchObject({ error: expect.stringContaining(`not ${key}`) });
        const loose = await store(JSON.stringify({ outputs: [heldOutput], key: key }));
        expect(await (await put(loose)).json()).toMatchObject({ error: expect.stringContaining('canonical') });
        const missingOutput = { path: 'gone', sha256: await freshKey(), bytes: 5 };
        const dangling = await store(canonicalManifest({ key: key, outputs: [heldOutput, missingOutput] }));
        const refused = await put(dangling);
        expect(refused.status).toBe(409);
        expect(await refused.json()).toMatchObject({ missing: ['gone'] });
        expect(await env.PublicStore.head(actionRef(key))).toBeNull();
        expect((await put(await store(canonicalManifest({ key: key, outputs: [heldOutput] })))).status).toBe(201);
    });

    it('is reached through loom-pipeline at /actions', async function () {
        const workshop = await token('workshop', 'build');
        const key = await freshKey();
        const manifest = await putProduct(workshop, key, { out: randomBytes(32) });
        const pipeline = (exports as unknown as { default: Fetcher }).default;
        const put = await pipeline.fetch(`${Origin}/actions/${key}`, {
            method: 'PUT',
            headers: { Authorization: `Bearer ${workshop}` },
            body: manifest,
        });
        expect(put.status).toBe(201);
        const submitter = await token('kirk', 'submit');
        expect((await pipeline.fetch(`${Origin}/actions/${key}`, { method: 'PUT', headers: { Authorization: `Bearer ${submitter}` }, body: manifest })).status).toBe(403);
    });

    it('routes only /actions, and only its two shapes and methods', async function () {
        const workshop = await token('workshop', 'build');
        const key = await freshKey();
        expect(await action('/public/blobs/' + key)).toBeNull();
        expect(await action('/actionsx')).toBeNull();
        expect((await answer('/actions', { bearer: workshop })).status).toBe(404);
        expect((await answer(`/actions/blobs/${key}/x`, { bearer: workshop })).status).toBe(404);
        expect((await answer('/actions/NOT-HEX', { method: 'PUT', bearer: workshop, body: key })).status).toBe(400);
        expect((await answer(`/actions/${key}`, { bearer: workshop })).status).toBe(405);
        expect((await answer(`/actions/blobs/${key}`, { bearer: workshop })).status).toBe(405);
        expect((await answer(`/actions/blobs/${key}`, { method: 'HEAD', bearer: workshop })).status).toBe(404);
    });
});

describe('an action manifest', function () {
    it('is canonical, so two honest builds of one key write the same bytes', async function () {
        const key = await freshKey();
        const a = { path: 'b/two', sha256: await freshKey(), bytes: 2 };
        const b = { path: 'a/one', sha256: await freshKey(), bytes: 1 };
        expect(canonicalManifest({ key: key, outputs: [a, b] })).toBe(canonicalManifest({ key: key, outputs: [b, a] }));
        expect(checkManifest(canonicalManifest({ key: key, outputs: [a, b] }), key)).toEqual({ key: key, outputs: [b, a] });
    });

    it('refuses paths outside the product, duplicates, bad hashes and sizes', async function () {
        const key = await freshKey();
        const sha256 = await freshKey();
        for (const path of ['', '/abs', '../up', 'a/../b', 'a//b', './a', 'a/']) {
            expect(typeof checkManifest(canonicalManifest({ key: key, outputs: [{ path: path, sha256: sha256, bytes: 1 }] }), key), path).toBe('string');
        }
        expect(typeof checkManifest(canonicalManifest({ key: key, outputs: [{ path: 'a', sha256: sha256, bytes: 1 }, { path: 'a', sha256: sha256, bytes: 1 }] }), key)).toBe('string');
        expect(typeof checkManifest(canonicalManifest({ key: key, outputs: [{ path: 'a', sha256: 'ABC', bytes: 1 }] }), key)).toBe('string');
        expect(typeof checkManifest(canonicalManifest({ key: key, outputs: [{ path: 'a', sha256: sha256, bytes: -1 }] }), key)).toBe('string');
        expect(typeof checkManifest(canonicalManifest({ key: key, outputs: [{ path: 'a', sha256: sha256, bytes: 1.5 }] }), key)).toBe('string');
        expect(typeof checkManifest('{"key":"' + key + '","outputs":[],"builder":"workshop"}', key)).toBe('string');
    });
});
