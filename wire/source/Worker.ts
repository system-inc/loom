// Loom's live wire: the endpoint table in docs/protocol.md. The Worker checks every token and hands run
// work to that run's Durable Object; blobs go straight to R2.

import { getBlob, putBlob, Sha256Pattern } from './Blobs';
import { jsonResponse } from './Http';
import { renderLivePage } from './LivePage';
import { RunHeader } from './RunObject';
import { verifyToken, type TokenClaims, type TokenScope } from './Token';

export { RunObject } from './RunObject';

// Run ids are not constrained by the protocol yet; these characters are safe in a URL path and an R2 key.
export const RunIdPattern = /^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$/;

const anyScope: TokenScope[] = ['runner', 'viewer', 'coordinator'];
const writerScopes: TokenScope[] = ['runner', 'coordinator'];
const coordinatorScope: TokenScope[] = ['coordinator'];

interface Grant {
    scopes: TokenScope[];
    // The page and its WebSocket come from a browser, which can't set an Authorization header.
    tokenInQuery: boolean;
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
    const authorization = request.headers.get('Authorization');
    if (authorization !== null) {
        const match = /^Bearer\s+(\S+)$/i.exec(authorization);
        if (match === null) {
            return jsonResponse(401, { error: 'Authorization must be Bearer <token>' });
        }
        token = match[1] ?? null;
    }
    else if (grant.tokenInQuery) {
        token = new URL(request.url).searchParams.get('token');
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
    headers.delete('Authorization');
    const inner = new Request(`https://run/${operation}${url.search}`, {
        method: request.method,
        headers: headers,
        body: request.body,
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
        const claims = await authorize(request, environment, run, { scopes: anyScope, tokenInQuery: true });
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
        const claims = await authorize(request, environment, run, { scopes: anyScope, tokenInQuery: true });
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
    const claims = await authorize(request, environment, run, { scopes: scopes, tokenInQuery: false });
    if (claims instanceof Response) {
        return claims;
    }
    return forward(environment, run, operation, request);
}

async function handleBlob(request: Request, environment: Env, sha256: string): Promise<Response> {
    if (request.method !== 'GET' && request.method !== 'PUT') {
        return methodNotAllowed('GET, PUT');
    }
    const claims = await authorize(request, environment, null, { scopes: writerScopes, tokenInQuery: false });
    if (claims instanceof Response) {
        return claims;
    }
    if (request.method === 'GET') {
        return getBlob(environment.Store, sha256);
    }
    return putBlob(environment.Store, sha256, request);
}

export default {
    async fetch(request: Request, environment: Env): Promise<Response> {
        const path = new URL(request.url).pathname;
        const runMatch = /^\/runs\/([^/]+)(?:\/([a-z]+))?\/?$/.exec(path);
        if (runMatch !== null) {
            const run = runMatch[1] ?? '';
            if (!RunIdPattern.test(run)) {
                return jsonResponse(400, { error: 'a run id is letters, digits, dot, dash and underscore' });
            }
            return handleRun(request, environment, run, runMatch[2] ?? '');
        }
        const blobMatch = /^\/blobs\/([^/]+)$/.exec(path);
        if (blobMatch !== null) {
            const sha256 = blobMatch[1] ?? '';
            if (!Sha256Pattern.test(sha256)) {
                return jsonResponse(400, { error: 'a blob is addressed by 64 lowercase hex digits' });
            }
            return handleBlob(request, environment, sha256);
        }
        if (path === '/') {
            return new Response('Loom wire. The endpoints are in docs/protocol.md.\n', {
                headers: { 'Content-Type': 'text/plain; charset=utf-8' },
            });
        }
        return jsonResponse(404, { error: 'no such endpoint' });
    },
} satisfies ExportedHandler<Env>;
