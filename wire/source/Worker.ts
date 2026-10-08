// Loom's live wire: the endpoint table in docs/protocol.md. The Worker checks every token and hands run
// work to that run's Durable Object. Blob bytes go straight to R2, but whether a token may reach a blob is
// the run object's call, since it holds the plan's inputs and the run's uploads.

import { getBlob, headBlob, putBlob, Sha256Pattern } from './Blobs';
import { getCacheEntry, putCacheEntry } from './Cache';
import { RunIdPattern } from './Events';
import { jsonResponse } from './Http';
import { renderLivePage } from './LivePage';
import { RunHeader, ScopeHeader } from './RunObject';
import { verifyToken, type TokenClaims, type TokenScope } from './Token';

export { RunObject } from './RunObject';

const anyScope: TokenScope[] = ['runner', 'viewer', 'coordinator'];
const writerScopes: TokenScope[] = ['runner', 'coordinator'];
const coordinatorScope: TokenScope[] = ['coordinator'];
const viewerScope: TokenScope[] = ['viewer'];

interface Grant {
    scopes: TokenScope[];
    // The scopes whose token may come as ?token=. The page and its WebSocket come from a browser, which can't
    // set an Authorization header, and so does a viewer's blob download link.
    queryScopes: TokenScope[];
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
    if (authorization !== null) {
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
        return new Response(renderLivePage(nonce), {
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
        return methodNotAllowed('POST');
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

// The cache belongs to no one run, so any run's coordinator token reaches it.
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
