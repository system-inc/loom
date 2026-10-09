// Loom's live wire: the endpoint table in docs/protocol.md. The Worker checks every token and hands run
// work to that run's Durable Object, board work to the one board object, and pool work to that pool's object.
// Blob bytes go straight to R2, but
// whether a token may reach a blob is the run object's call, since it holds the plan's inputs and the run's uploads.

import { BoardName, BoardSubprotocol } from './Board';
import { getBlob, headBlob, putBlob, Sha256Pattern } from './Blobs';
import { getCacheEntry, putCacheEntry } from './Cache';
import { RunIdPattern } from './Events';
import { jsonResponse } from './Http';
import { renderBoardPage } from './BoardPage';
import { renderLivePage } from './LivePage';
import { RunHeader, ScopeHeader } from './RunObject';
import { mintToken, verifyToken, type TokenClaims, type TokenScope } from './Token';

export { Board } from './Board';
export { Pool } from './Pool';
export { RunObject } from './RunObject';

// Every scope of a run. A board token is none of them: it watches the board and reaches no run endpoint. Nor is a
// pool token, which reaches its own pool's next and nothing else.
const anyScope: TokenScope[] = ['runner', 'viewer', 'coordinator'];
const writerScopes: TokenScope[] = ['runner', 'coordinator'];
const coordinatorScope: TokenScope[] = ['coordinator'];
const viewerScope: TokenScope[] = ['viewer'];
const boardScope: TokenScope[] = ['board'];
const poolScope: TokenScope[] = ['pool'];
// A gate box writing build products to the public store holds a publish token: it writes /public and nothing else.
const publicWriterScopes: TokenScope[] = ['coordinator', 'publish'];
const watcherScopes: TokenScope[] = ['coordinator', 'board'];
const subprotocolTokenPrefix = 'token.';

interface Grant {
    scopes: TokenScope[];
    // The scopes whose token may come as ?token=. The page and its WebSocket come from a browser, which can't
    // set an Authorization header, and so does a viewer's blob download link.
    queryScopes: TokenScope[];
    // The board's WebSocket takes its token only as the subprotocol token.<token>, so it is never in a URL.
    subprotocol?: boolean;
}

// The subprotocols a WebSocket request offers, in order.
function offeredSubprotocols(request: Request): string[] {
    const header = request.headers.get('Sec-WebSocket-Protocol');
    if (header === null) {
        return [];
    }
    return header
        .split(',')
        .map(function (protocol) {
            return protocol.trim();
        })
        .filter(function (protocol) {
            return protocol !== '';
        });
}

// Finds the token, checks it, and checks its scope and run. Returns the claims or the refusal.
async function authorize(
    request: Request,
    environment: Env,
    run: string | null,
    grant: Grant,
): Promise<TokenClaims | Response> {
    const secret = environment.LOOM_TOKEN_SECRET;
    if (typeof secret !== 'string' || secret === '') {
        return jsonResponse(500, { error: 'the wire has no token secret' });
    }
    let token: string | null = null;
    let fromQuery = false;
    const authorization = request.headers.get('Authorization');
    if (grant.subprotocol === true) {
        const offered = offeredSubprotocols(request).find(function (protocol) {
            return protocol.startsWith(subprotocolTokenPrefix);
        });
        token = offered === undefined ? null : offered.slice(subprotocolTokenPrefix.length);
    }
    else if (authorization !== null) {
        const match = /^Bearer\s+(\S+)$/i.exec(authorization);
        if (match === null) {
            return jsonResponse(401, { error: 'Authorization must be Bearer <token>' });
        }
        token = match[1] ?? null;
    }
    else if (grant.queryScopes.length > 0) {
        token = new URL(request.url).searchParams.get('token');
        fromQuery = true;
    }
    if (token === null || token === '') {
        return jsonResponse(401, { error: 'a run token is required' }, { 'WWW-Authenticate': 'Bearer' });
    }
    const verification = await verifyToken(secret, token, Math.floor(Date.now() / 1000));
    if (!verification.valid) {
        return jsonResponse(401, { error: verification.reason }, { 'WWW-Authenticate': 'Bearer' });
    }
    if (!grant.scopes.includes(verification.claims.scope)) {
        return jsonResponse(403, { error: `a ${verification.claims.scope} token can't do this` });
    }
    if (fromQuery && !grant.queryScopes.includes(verification.claims.scope)) {
        return jsonResponse(
            401,
            { error: `a ${verification.claims.scope} token goes in the Authorization header here` },
            { 'WWW-Authenticate': 'Bearer' },
        );
    }
    if (run !== null && verification.claims.run !== run) {
        return jsonResponse(403, { error: `this token is for run ${verification.claims.run}, not ${run}` });
    }
    return verification.claims;
}

function runObject(environment: Env, run: string): DurableObjectStub {
    return environment.Runs.get(environment.Runs.idFromName(run));
}

// Forwards to the run's object with the run named in a header the outside can't reach.
function forward(environment: Env, run: string, operation: string, request: Request): Promise<Response> {
    const url = new URL(request.url);
    const headers = new Headers(request.headers);
    headers.set(RunHeader, run);
    headers.delete(ScopeHeader);
    headers.delete('Authorization');
    const inner = new Request(`https://run/${operation}${url.search}`, {
        method: request.method,
        headers: headers,
        body: request.body,
    });
    return runObject(environment, run).fetch(inner);
}

// An internal question for the run's object about one blob, with the token's scope beside the run.
function askRun(environment: Env, run: string, operation: string, scope: TokenScope, body: unknown): Promise<Response> {
    const inner = new Request(`https://run/${operation}`, {
        method: 'POST',
        headers: { [RunHeader]: run, [ScopeHeader]: scope, 'Content-Type': 'application/json' },
        body: JSON.stringify(body),
    });
    return runObject(environment, run).fetch(inner);
}

// A page of ours: inline styles and script under the response's nonce, nothing from elsewhere, and only
// this origin's own endpoints and WebSocket to talk to.
function pageResponse(html: string, nonce: string, host: string): Response {
    return new Response(html, {
        headers: {
            'Content-Type': 'text/html; charset=utf-8',
            'Cache-Control': 'no-store',
            'Referrer-Policy': 'no-referrer',
            'X-Content-Type-Options': 'nosniff',
            'Content-Security-Policy': [
                "default-src 'none'",
                `script-src 'nonce-${nonce}'`,
                `style-src 'nonce-${nonce}'`,
                `connect-src 'self' wss://${host} ws://${host}`,
                "base-uri 'none'",
                "form-action 'none'",
                "frame-ancestors 'none'",
            ].join('; '),
        },
    });
}

function methodNotAllowed(allowed: string): Response {
    return jsonResponse(405, { error: `use ${allowed}` }, { Allow: allowed });
}

async function handleRun(request: Request, environment: Env, run: string, operation: string): Promise<Response> {
    if (operation === '') {
        if (request.method !== 'GET') {
            return methodNotAllowed('GET');
        }
        const claims = await authorize(request, environment, run, { scopes: anyScope, queryScopes: anyScope });
        if (claims instanceof Response) {
            return claims;
        }
        const nonce = crypto.randomUUID().replace(/-/g, '');
        const host = new URL(request.url).host;
        return pageResponse(renderLivePage(nonce), nonce, host);
    }
    if (operation === 'stream') {
        if (request.method !== 'GET') {
            return methodNotAllowed('GET');
        }
        const claims = await authorize(request, environment, run, { scopes: anyScope, queryScopes: anyScope });
        if (claims instanceof Response) {
            return claims;
        }
        return forward(environment, run, 'stream', request);
    }
    if (operation === 'events' && request.method === 'GET') {
        // The coordinator follows a pool unit's stream here, by plain HTTP, after a position it has read up to.
        const claims = await authorize(request, environment, run, { scopes: coordinatorScope, queryScopes: [] });
        if (claims instanceof Response) {
            return claims;
        }
        return forward(environment, run, 'events', request);
    }
    const grants: Record<string, TokenScope[]> = {
        plan: coordinatorScope,
        events: writerScopes,
        verdict: coordinatorScope,
    };
    const scopes = grants[operation];
    if (scopes === undefined) {
        return jsonResponse(404, { error: 'no such endpoint' });
    }
    if (request.method !== 'POST') {
        return methodNotAllowed(operation === 'events' ? 'GET, POST' : 'POST');
    }
    const claims = await authorize(request, environment, run, { scopes: scopes, queryScopes: [] });
    if (claims instanceof Response) {
        return claims;
    }
    return forward(environment, run, operation, request);
}

// A blob through its run: the token must be the path's run, the run object decides whether its scope may
// reach this hash (403) or write now (409), and only then does R2 answer (404 when it doesn't hold the blob).
async function handleBlob(request: Request, environment: Env, run: string, sha256: string): Promise<Response> {
    if (request.method !== 'GET' && request.method !== 'HEAD' && request.method !== 'PUT') {
        return methodNotAllowed('GET, HEAD, PUT');
    }
    const claims = await authorize(request, environment, run, { scopes: anyScope, queryScopes: viewerScope });
    if (claims instanceof Response) {
        return claims;
    }
    const action = request.method === 'PUT' ? 'write' : 'read';
    const access = await askRun(environment, run, 'blob-access', claims.scope, { sha256: sha256, action: action });
    if (access.status !== 200) {
        await request.body?.cancel();
        return access;
    }
    if (request.method === 'GET') {
        return getBlob(environment.Store, sha256);
    }
    if (request.method === 'HEAD') {
        // Asking proves no bytes, so it records nothing.
        return headBlob(environment.Store, sha256);
    }
    const put = await putBlob(environment.Store, sha256, request);
    if (put.bytes === null) {
        return put.response;
    }
    const recorded = await askRun(environment, run, 'blob-uploaded', claims.scope, { sha256: sha256, bytes: put.bytes });
    if (recorded.status !== 200) {
        return recorded;
    }
    return put.response;
}

function boardObject(environment: Env): DurableObjectStub {
    return environment.Board.get(environment.Board.idFromName(BoardName));
}

// Forwards to the board object at a fixed operation; the Worker names it, so the outside can't reach /run, the
// door each run's object pushes its summary through.
function forwardToBoard(environment: Env, operation: string, request: Request): Promise<Response> {
    const headers = new Headers(request.headers);
    headers.delete('Authorization');
    if (headers.has('Sec-WebSocket-Protocol')) {
        // The token has done its work here; the board object only needs to know what to answer.
        headers.set('Sec-WebSocket-Protocol', BoardSubprotocol);
    }
    const inner = new Request(`https://board/${operation}`, {
        method: request.method,
        headers: headers,
        body: request.body,
    });
    return boardObject(environment).fetch(inner);
}

// The board: its stream and snapshot for a board token, the machines from any run's coordinator.
async function handleBoard(request: Request, environment: Env, operation: string): Promise<Response> {
    if (operation === 'stream') {
        if (request.method !== 'GET') {
            return methodNotAllowed('GET');
        }
        const claims = await authorize(request, environment, BoardName, {
            scopes: boardScope,
            queryScopes: [],
            subprotocol: true,
        });
        if (claims instanceof Response) {
            return claims;
        }
        // The answer must name a subprotocol the browser offered, and the page offers loom beside its token.
        if (!offeredSubprotocols(request).includes(BoardSubprotocol)) {
            return jsonResponse(400, { error: `offer the ${BoardSubprotocol} subprotocol beside the token` });
        }
        return forwardToBoard(environment, 'stream', request);
    }
    if (operation === 'snapshot') {
        if (request.method !== 'GET') {
            return methodNotAllowed('GET');
        }
        const claims = await authorize(request, environment, BoardName, { scopes: boardScope, queryScopes: [] });
        if (claims instanceof Response) {
            return claims;
        }
        return forwardToBoard(environment, 'snapshot', request);
    }
    if (operation === 'gate') {
        if (request.method !== 'POST') {
            return methodNotAllowed('POST');
        }
        // The gate's lines come from the reader on Kirk's Mac, with a coordinator token like the machines.
        const claims = await authorize(request, environment, null, { scopes: coordinatorScope, queryScopes: [] });
        if (claims instanceof Response) {
            return claims;
        }
        return forwardToBoard(environment, 'gate', request);
    }
    if (operation === 'machines') {
        if (request.method !== 'POST') {
            return methodNotAllowed('POST');
        }
        // Like the cache, the machines belong to no one run, so any run's coordinator token posts them.
        const claims = await authorize(request, environment, null, { scopes: coordinatorScope, queryScopes: [] });
        if (claims instanceof Response) {
            return claims;
        }
        return forwardToBoard(environment, 'machines', request);
    }
    return jsonResponse(404, { error: 'no such endpoint' });
}

// A board token asks for a viewer token to one run, to open that run's page and stream. It expires when the board
// token does, so it can never outlive the token that asked for it.
async function handleBoardViewer(request: Request, environment: Env, run: string): Promise<Response> {
    if (request.method !== 'POST') {
        return methodNotAllowed('POST');
    }
    const claims = await authorize(request, environment, BoardName, { scopes: boardScope, queryScopes: [] });
    if (claims instanceof Response) {
        return claims;
    }
    const viewer = await mintToken(environment.LOOM_TOKEN_SECRET, {
        run: run,
        scope: 'viewer',
        expires: claims.expires,
    });
    return jsonResponse(200, { token: viewer });
}

// Forwards to the pool's object at a fixed operation, so the outside names only the endpoints below.
function forwardToPool(environment: Env, pool: string, operation: string, request: Request): Promise<Response> {
    const headers = new Headers(request.headers);
    headers.delete('Authorization');
    const inner = new Request(`https://pool/${operation}`, {
        method: request.method,
        headers: headers,
        body: request.body,
    });
    return environment.Pools.get(environment.Pools.idFromName(pool)).fetch(inner);
}

// A pool (docs/protocol.md, The pool). The units, the cancel, the queued and the pool's state belong to no one run, so any
// run's coordinator token reaches them, and a board token may watch. Only the pool's own pool token asks for next.
async function handlePool(request: Request, environment: Env, pool: string, operation: string): Promise<Response> {
    if (operation === '') {
        if (request.method !== 'GET') {
            return methodNotAllowed('GET');
        }
        const claims = await authorize(request, environment, null, { scopes: watcherScopes, queryScopes: [] });
        if (claims instanceof Response) {
            return claims;
        }
        return forwardToPool(environment, pool, 'status', request);
    }
    if (operation !== 'units' && operation !== 'next' && operation !== 'cancel' && operation !== 'queued') {
        return jsonResponse(404, { error: 'no such endpoint' });
    }
    if (request.method !== 'POST') {
        return methodNotAllowed('POST');
    }
    // A pool token's run is its pool's name, so a token for another pool is refused like a token for another run.
    const claims =
        operation === 'next'
            ? await authorize(request, environment, pool, { scopes: poolScope, queryScopes: [] })
            : await authorize(request, environment, null, { scopes: coordinatorScope, queryScopes: [] });
    if (claims instanceof Response) {
        return claims;
    }
    return forwardToPool(environment, pool, operation, request);
}

// The cache belongs to no one run, so any run's coordinator token reaches it.
// The public store (adamic-public): anyone reads it direct at adamic-store.kirkouimet.com/blobs/<sha256>, so a
// hundred instances fetch from Cloudflare's edge, never through this Worker. Only a coordinator token of any
// run writes it, and a write is held to the same sha256 check as every blob, so nobody can poison or fill it.
async function handlePublicBlob(request: Request, environment: Env, sha256: string): Promise<Response> {
    if (request.method !== 'HEAD' && request.method !== 'PUT') {
        return methodNotAllowed('HEAD, PUT');
    }
    const claims = await authorize(request, environment, null, { scopes: publicWriterScopes, queryScopes: [] });
    if (claims instanceof Response) {
        return claims;
    }
    if (request.method === 'HEAD') {
        return headBlob(environment.PublicStore, sha256);
    }
    return (await putBlob(environment.PublicStore, sha256, request)).response;
}

// A named ref in the public store: refs/<namespace>/<name> holds one blob's sha256 (64 hex digits), read direct
// and unauthenticated at adamic-store.kirkouimet.com/refs/<namespace>/<name>. The build cache keeps a product's
// manifest under its cache key this way (@system_adamic, Oct 9). A ref is written only through here, by a
// coordinator or publish token, and only to a blob the store already holds, so a ref never dangles. A cache key
// is honest, so the same key always names the same product: a write that would change a ref is refused (409),
// which also surfaces a key that isn't honest.
export const RefNamespacePattern = /^[a-z][a-z0-9-]{0,31}$/;
export const RefNamePattern = /^[A-Za-z0-9][A-Za-z0-9._-]{0,199}$/;

async function handlePublicRef(request: Request, environment: Env, namespace: string, name: string): Promise<Response> {
    if (request.method !== 'PUT') {
        return methodNotAllowed('PUT');
    }
    const claims = await authorize(request, environment, null, { scopes: publicWriterScopes, queryScopes: [] });
    if (claims instanceof Response) {
        return claims;
    }
    const target = (await request.text()).trim();
    if (!Sha256Pattern.test(target)) {
        return jsonResponse(400, { error: 'a ref holds one blob sha256, 64 lowercase hex digits' });
    }
    if ((await environment.PublicStore.head(`blobs/${target}`)) === null) {
        return jsonResponse(409, { error: `blob ${target} isn't in the store; put it before its ref` });
    }
    const key = `refs/${namespace}/${name}`;
    const existing = await environment.PublicStore.get(key);
    if (existing !== null) {
        const held = (await existing.text()).trim();
        if (held === target) {
            return jsonResponse(200, { ref: key, sha256: target, created: false });
        }
        return jsonResponse(409, { error: `${key} already names ${held}; a ref never changes`, held });
    }
    await environment.PublicStore.put(key, target, { httpMetadata: { contentType: 'text/plain' } });
    return jsonResponse(201, { ref: key, sha256: target, created: true });
}

async function handleCache(request: Request, environment: Env, key: string): Promise<Response> {
    if (request.method !== 'GET' && request.method !== 'PUT') {
        return methodNotAllowed('GET, PUT');
    }
    const claims = await authorize(request, environment, null, { scopes: coordinatorScope, queryScopes: [] });
    if (claims instanceof Response) {
        return claims;
    }
    if (request.method === 'GET') {
        return getCacheEntry(environment.Store, key);
    }
    return putCacheEntry(environment.Store, key, request);
}

export default {
    async fetch(request: Request, environment: Env): Promise<Response> {
        const path = new URL(request.url).pathname;
        const publicMatch = /^\/public\/blobs\/([^/]+)$/.exec(path);
        if (publicMatch !== null) {
            const sha256 = publicMatch[1] ?? '';
            if (!Sha256Pattern.test(sha256)) {
                return jsonResponse(400, { error: 'a blob is addressed by 64 lowercase hex digits' });
            }
            return handlePublicBlob(request, environment, sha256);
        }
        const refMatch = /^\/public\/refs\/([^/]+)\/([^/]+)$/.exec(path);
        if (refMatch !== null) {
            const namespace = refMatch[1] ?? '';
            const name = refMatch[2] ?? '';
            if (!RefNamespacePattern.test(namespace) || !RefNamePattern.test(name)) {
                return jsonResponse(400, { error: 'a ref is /public/refs/<namespace: lowercase>/<name: letters, digits, dot, dash, underscore>' });
            }
            return handlePublicRef(request, environment, namespace, name);
        }
        const blobMatch = /^\/runs\/([^/]+)\/blobs\/([^/]+)$/.exec(path);
        if (blobMatch !== null) {
            const run = blobMatch[1] ?? '';
            const sha256 = blobMatch[2] ?? '';
            if (!RunIdPattern.test(run)) {
                return jsonResponse(400, { error: 'a run id is letters, digits, dot, dash and underscore' });
            }
            if (!Sha256Pattern.test(sha256)) {
                return jsonResponse(400, { error: 'a blob is addressed by 64 lowercase hex digits' });
            }
            return handleBlob(request, environment, run, sha256);
        }
        const runMatch = /^\/runs\/([^/]+)(?:\/([a-z]+))?\/?$/.exec(path);
        if (runMatch !== null) {
            const run = runMatch[1] ?? '';
            if (!RunIdPattern.test(run)) {
                return jsonResponse(400, { error: 'a run id is letters, digits, dot, dash and underscore' });
            }
            return handleRun(request, environment, run, runMatch[2] ?? '');
        }
        const boardViewerMatch = /^\/board\/runs\/([^/]+)\/viewer$/.exec(path);
        if (boardViewerMatch !== null) {
            const run = boardViewerMatch[1] ?? '';
            if (!RunIdPattern.test(run)) {
                return jsonResponse(400, { error: 'a run id is letters, digits, dot, dash and underscore' });
            }
            return handleBoardViewer(request, environment, run);
        }
        if (path === '/board' || path === '/board/') {
            // The page carries no data and no token: the board token stays after the # and reaches the
            // stream only as a WebSocket subprotocol.
            if (request.method !== 'GET') {
                return methodNotAllowed('GET');
            }
            const nonce = crypto.randomUUID().replace(/-/g, '');
            return pageResponse(renderBoardPage(nonce), nonce, new URL(request.url).host);
        }
        const boardMatch = /^\/board\/([a-z]+)$/.exec(path);
        if (boardMatch !== null) {
            return handleBoard(request, environment, boardMatch[1] ?? '');
        }
        const poolMatch = /^\/pools\/([^/]+)(?:\/([a-z]+))?\/?$/.exec(path);
        if (poolMatch !== null) {
            const pool = poolMatch[1] ?? '';
            if (!RunIdPattern.test(pool)) {
                return jsonResponse(400, { error: 'a pool name is letters, digits, dot, dash and underscore' });
            }
            return handlePool(request, environment, pool, poolMatch[2] ?? '');
        }
        const cacheMatch = /^\/cache\/([^/]+)$/.exec(path);
        if (cacheMatch !== null) {
            const key = cacheMatch[1] ?? '';
            if (!Sha256Pattern.test(key)) {
                return jsonResponse(400, { error: 'a cache key is 64 lowercase hex digits' });
            }
            return handleCache(request, environment, key);
        }
        if (path === '/') {
            return new Response('Loom wire. The endpoints are in docs/protocol.md.\n', {
                headers: { 'Content-Type': 'text/plain; charset=utf-8' },
            });
        }
        return jsonResponse(404, { error: 'no such endpoint' });
    },
} satisfies ExportedHandler<Env>;
