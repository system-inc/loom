// The action store (contracts v1.1, #ykg8g6k): Workshop builds each action once, keyed by its productKey, and
// writes its outputs here. refs/action/<productKey> in the public store (adamic-public) holds the sha256 of one
// manifest blob, and the manifest lists the outputs. Anyone reads it direct and unauthenticated at
// adamic-store.kirkouimet.com/refs/action/<productKey>, then the manifest, then each blob, checking every hash.
//
// Only a build token writes here, and a build token writes nothing else, so every action a runner fetches was built
// by a builder we named. Its run claim is the builder's name (workshop), and the ref keeps it. A ref is written
// once: the same key naming other bytes is a key that isn't honest or a build that isn't reproducible, and the 409
// names both builders so the two builds can be compared.
//
// The manifest is canonical JSON with no builder and no time in it, so two honest builds of one key write the
// same manifest bytes and the same sha256:
//   {"key":"<productKey>","outputs":[{"bytes":<n>,"executable":<bool>,"path":"<relative path>","sha256":"<64 hex>"}, ...]}
// keys sorted, outputs sorted by path, no whitespace. A file is executable or not, never a mode, so a builder's umask
// can't make two honest builds differ.

import { blobExists, headBlob, putBlob, Sha256Pattern } from './Blobs';
import { jsonResponse, readBodyText } from './Http';
import { verifyToken, type TokenClaims } from './Token';

export const ActionRefPrefix = 'refs/action/';
export const BuilderMetadata = 'builder';
export const MaximumManifestBytes = 1024 * 1024;
// A blob is named by its sha256 and never replaced, so the edge and every reader may keep it forever (#k62gwdt).
export const ImmutableBlob = 'public, max-age=31536000, immutable';

export interface ActionEnvironment {
    PublicStore: R2Bucket;
    LOOM_TOKEN_SECRET?: string;
}

export interface ActionOutput {
    path: string;
    sha256: string;
    bytes: number;
    executable: boolean;
}

export interface ActionManifest {
    key: string;
    outputs: ActionOutput[];
}

export function actionRef(productKey: string): string {
    return ActionRefPrefix + productKey;
}

// The canonical bytes of a manifest: what a builder uploads and what checkManifest accepts.
export function canonicalManifest(manifest: ActionManifest): string {
    const outputs = [...manifest.outputs].sort(function (left, right) {
        return left.path < right.path ? -1 : left.path > right.path ? 1 : 0;
    });
    return JSON.stringify({
        key: manifest.key,
        outputs: outputs.map(function (output) {
            return { bytes: output.bytes, executable: output.executable, path: output.path, sha256: output.sha256 };
        }),
    });
}

// A manifest is exactly key and outputs, for this key, each output a relative path inside the product (no `..`, no
// leading slash, no duplicates), a sha256 and a size, in canonical form.
export function checkManifest(text: string, productKey: string): ActionManifest | string {
    let parsed: unknown;
    try {
        parsed = JSON.parse(text);
    }
    catch {
        return 'the manifest is not JSON';
    }
    if (typeof parsed !== 'object' || parsed === null || Array.isArray(parsed)) {
        return 'a manifest is an object';
    }
    const record = parsed as Record<string, unknown>;
    if (Object.keys(record).sort().join(',') !== 'key,outputs') {
        return 'a manifest has exactly key and outputs';
    }
    if (record.key !== productKey) {
        return `the manifest is for ${String(record.key)}, not ${productKey}`;
    }
    if (!Array.isArray(record.outputs)) {
        return 'outputs is a list';
    }
    const outputs: ActionOutput[] = [];
    const seen = new Set<string>();
    for (const value of record.outputs as unknown[]) {
        if (typeof value !== 'object' || value === null || Array.isArray(value)) {
            return 'each output is an object';
        }
        const output = value as Record<string, unknown>;
        if (Object.keys(output).sort().join(',') !== 'bytes,executable,path,sha256') {
            return 'each output is exactly a path, a sha256, a size in bytes and whether it is executable';
        }
        if (
            typeof output.path !== 'string' ||
            output.path === '' ||
            output.path.split('/').some(function (part) {
                return part === '' || part === '.' || part === '..';
            })
        ) {
            return `an output path is relative and inside the product: ${JSON.stringify(output.path)}`;
        }
        if (seen.has(output.path)) {
            return `${output.path} is listed twice`;
        }
        seen.add(output.path);
        if (typeof output.sha256 !== 'string' || !Sha256Pattern.test(output.sha256)) {
            return `${output.path}'s sha256 is 64 lowercase hex digits`;
        }
        if (typeof output.bytes !== 'number' || !Number.isSafeInteger(output.bytes) || output.bytes < 0) {
            return `${output.path}'s size is a whole number of bytes`;
        }
        if (typeof output.executable !== 'boolean') {
            return `${output.path}'s executable is true or false`;
        }
        outputs.push({ path: output.path, sha256: output.sha256, bytes: output.bytes, executable: output.executable });
    }
    const manifest = { key: productKey, outputs: outputs };
    if (canonicalManifest(manifest) !== text) {
        return 'the manifest is not in canonical form (keys sorted, outputs sorted by path, no whitespace)';
    }
    return manifest;
}

async function authorizeBuilder(request: Request, environment: ActionEnvironment): Promise<TokenClaims | Response> {
    const secret = environment.LOOM_TOKEN_SECRET;
    if (typeof secret !== 'string' || secret === '') {
        return jsonResponse(500, { error: 'the pipeline has no token secret' });
    }
    const match = /^Bearer\s+(\S+)$/i.exec(request.headers.get('Authorization') ?? '');
    if (match === null) {
        return jsonResponse(401, { error: 'a build token is required' }, { 'WWW-Authenticate': 'Bearer' });
    }
    const verification = await verifyToken(secret, match[1] ?? '', Math.floor(Date.now() / 1000));
    if (!verification.valid) {
        return jsonResponse(401, { error: verification.reason }, { 'WWW-Authenticate': 'Bearer' });
    }
    if (verification.claims.scope !== 'build') {
        return jsonResponse(403, { error: `refs/action is written by a build token only, not a ${verification.claims.scope} token` });
    }
    return verification.claims;
}

// Routes /actions/blobs/<sha256> (HEAD, PUT), /actions/<productKey> (PUT) and /actions/list (GET). Anything else under /actions is a 404;
// a path outside /actions is null, for the Worker's other routes.
export async function handleAction(request: Request, environment: ActionEnvironment): Promise<Response | null> {
    const path = new URL(request.url).pathname;
    if (path !== '/actions' && !path.startsWith('/actions/')) {
        return null;
    }
    if (path === '/actions/list') {
        return listActions(request, environment);
    }
    const blobMatch = /^\/actions\/blobs\/([^/]+)$/.exec(path);
    const refMatch = /^\/actions\/([^/]+)$/.exec(path);
    if (blobMatch === null && refMatch === null) {
        return jsonResponse(404, { error: 'the action store is /actions/blobs/<sha256> and /actions/<productKey>' });
    }
    const hash = (blobMatch ?? refMatch)?.[1] ?? '';
    if (!Sha256Pattern.test(hash)) {
        return jsonResponse(400, { error: 'a blob and a product key are 64 lowercase hex digits' });
    }
    const allowed = blobMatch !== null ? ['HEAD', 'PUT'] : ['PUT'];
    if (!allowed.includes(request.method)) {
        return jsonResponse(405, { error: `${allowed.join(', ')} only; reads go direct to the public store` }, { Allow: allowed.join(', ') });
    }
    const claims = await authorizeBuilder(request, environment);
    if (claims instanceof Response) {
        return claims;
    }
    if (blobMatch !== null) {
        if (request.method === 'HEAD') {
            return headBlob(environment.PublicStore, hash);
        }
        return (await putBlob(environment.PublicStore, hash, request, ImmutableBlob)).response;
    }
    return putActionRef(environment.PublicStore, hash, claims.run, request);
}

async function putActionRef(store: R2Bucket, productKey: string, builder: string, request: Request): Promise<Response> {
    const target = ((await readBodyText(request, 1024)) ?? '').trim();
    if (!Sha256Pattern.test(target)) {
        return jsonResponse(400, { error: "an action ref holds its manifest's sha256, 64 lowercase hex digits" });
    }
    const manifestObject = await store.get(`blobs/${target}`);
    if (manifestObject === null) {
        return jsonResponse(409, { error: `manifest ${target} isn't in the store; put it and its outputs first` });
    }
    if (manifestObject.size > MaximumManifestBytes) {
        return jsonResponse(400, { error: `a manifest is at most ${MaximumManifestBytes} bytes` });
    }
    const manifest = checkManifest(await manifestObject.text(), productKey);
    if (typeof manifest === 'string') {
        return jsonResponse(400, { error: manifest });
    }
    const held = await Promise.all(
        manifest.outputs.map(function (output) {
            return blobExists(store, output.sha256);
        }),
    );
    const missing = manifest.outputs.filter(function (_, index) {
        return !held[index];
    });
    if (missing.length > 0) {
        return jsonResponse(409, {
            error: "an action's outputs must be in the store before its ref",
            missing: missing.map(function (output) {
                return output.path;
            }),
        });
    }
    const key = actionRef(productKey);
    // Written once: R2 refuses the put when anything is already there, so of two builders racing, one wins and the
    // other is answered from what the winner wrote.
    const written = await store.put(key, target, {
        onlyIf: { etagDoesNotMatch: '*' },
        httpMetadata: { contentType: 'text/plain' },
        customMetadata: { [BuilderMetadata]: builder },
    });
    if (written !== null) {
        return jsonResponse(201, { ref: key, sha256: target, builder: builder, created: true });
    }
    const existing = await store.get(key);
    if (existing === null) {
        return jsonResponse(502, { error: `the store refused ${key} and holds nothing there` });
    }
    const heldTarget = (await existing.text()).trim();
    const heldBuilder = existing.customMetadata?.[BuilderMetadata] ?? 'unknown';
    if (heldTarget === target) {
        return jsonResponse(200, { ref: key, sha256: target, builder: heldBuilder, created: false });
    }
    return jsonResponse(409, {
        error: `${key}: ${heldBuilder} built ${heldTarget} and ${builder} built ${target}; an action's outputs never change`,
        held: heldTarget,
        heldBuilder: heldBuilder,
        builder: builder,
        sha256: target,
    });
}

// One page of what the action store holds, for Workshop's daily audit of its index (#k62gwdt): `GET
// /actions/list?prefix=refs|blobs[&cursor=<c>]` answers `{ keys, cursor }`, the action refs' product keys or the
// blobs' sha256s, at most 1,000 a page, and cursor null on the last. A build token only, like every write here.
async function listActions(request: Request, environment: ActionEnvironment): Promise<Response> {
    if (request.method !== 'GET') {
        return jsonResponse(405, { error: 'GET only' }, { Allow: 'GET' });
    }
    const claims = await authorizeBuilder(request, environment);
    if (claims instanceof Response) {
        return claims;
    }
    const parameters = new URL(request.url).searchParams;
    const which = parameters.get('prefix');
    if (which !== 'refs' && which !== 'blobs') {
        return jsonResponse(400, { error: 'prefix is refs or blobs' });
    }
    const prefix = which === 'refs' ? ActionRefPrefix : 'blobs/';
    const cursor = parameters.get('cursor');
    const listed = await environment.PublicStore.list({ prefix: prefix, limit: 1000, cursor: cursor === null || cursor === '' ? undefined : cursor });
    return jsonResponse(200, {
        keys: listed.objects.map(function (object) {
            return object.key.slice(prefix.length);
        }),
        cursor: listed.truncated ? listed.cursor : null,
    });
}
