// loom-pipeline's front door (docs/contracts.md, section 5): an owner submits a change with their submit token,
// anyone with a reason reads one, and the Mac's bridge reads what owners act on; its board shows a line per change.
// The tokens are checked by the same verifier as loom-wire's; Changes.ts checks the rest and asks the Queue object.

import { handleAction } from './Actions';
import { ChangeBoardName, changeBoardOf, ChangeBoardSubprotocol } from './ChangeBoard';
import { renderChangeBoardPage } from './ChangeBoardPage';
import { handleChanges, queueOf } from './Changes';
import { jsonResponse } from './Http';
import type { TokenScope } from './Token';
import { authorize, pageResponse } from './Worker';

export { ChangeBoard } from './ChangeBoard';

export { Queue } from './Queue';

// A change is submitted with its owner's submit token; any of these reads one, and Changes.ts says which may do what.
const changeScopes: TokenScope[] = ['submit', 'coordinator', 'board'];
// GET /verdicts/<unitKey>, POST /verdicts, GET /futures, POST /futures/<tree>/plan and /verdicts, GET /landings and
// POST /landings/<change>, GET /submissions and POST /submissions/<change>/facts.
const queueSeamPattern =
    /^\/(?:verdicts(?:\/[0-9a-f]{64})?|futures(?:\/[0-9a-f]{40}\/(?:plan|verdicts))?|landings(?:\/chg_[0-9a-z]{26})?|submissions(?:\/chg_[0-9a-z]{26}\/facts)?)$/;

export default {
    async fetch(request: Request, environment: Env): Promise<Response> {
        const path = new URL(request.url).pathname;
        const changesMatch = /^\/changes(?:\/(.+))?$/.exec(path);
        if (changesMatch !== null) {
            const claims = await authorize(request, environment, null, { scopes: changeScopes, queryScopes: [] });
            if (claims instanceof Response) {
                await request.body?.cancel();
                return claims;
            }
            return handleChanges(request, claims, changesMatch[1] ?? '', queueOf(environment));
        }
        // The coordinator's seams with the queue (contracts v1.1): the verdict index and the planner's futures, the
        // judge's verdicts, the landing orders the pusher pulls and answers, and git's facts for each submitted change
        // (no GitHub credential lives here). The Queue object checks the rest.
        if (queueSeamPattern.test(path)) {
            const claims = await authorize(request, environment, null, { scopes: ['coordinator'], queryScopes: [] });
            if (claims instanceof Response) {
                await request.body?.cancel();
                return claims;
            }
            const queue = queueOf(environment);
            if (queue === null) {
                await request.body?.cancel();
                return jsonResponse(503, { error: "the queue isn't on the wire yet" });
            }
            const url = new URL(request.url);
            // Only the body's type crosses: the caller's token stays at the door.
            const headers = { 'Content-Type': request.headers.get('Content-Type') ?? 'application/json' };
            return queue.fetch(new Request(`https://queue${path}${url.search}`, { method: request.method, headers: headers, body: request.body }));
        }
        // The action store checks its own build token, since no other scope may reach it.
        const action = await handleAction(request, environment);
        if (action !== null) {
            return action;
        }
        // The board of changes: its page holds no data and no token; its one read needs a board token. Only the
        // Queue object reaches the board's push, through its binding, never from outside.
        if (path === '/board' || path === '/board/') {
            if (request.method !== 'GET') {
                return jsonResponse(405, { error: 'use GET' }, { Allow: 'GET' });
            }
            const nonce = crypto.randomUUID().replace(/-/g, '');
            return pageResponse(renderChangeBoardPage(nonce), nonce, new URL(request.url).host);
        }
        // The page's live view: the board token comes only as the subprotocol token.<token>, beside loom, so it is in no
        // URL a server logs; the board object answers with loom alone.
        if (path === '/board/stream') {
            if (request.method !== 'GET') {
                return jsonResponse(405, { error: 'use GET' }, { Allow: 'GET' });
            }
            const claims = await authorize(request, environment, ChangeBoardName, { scopes: ['board'], queryScopes: [], subprotocol: true });
            if (claims instanceof Response) {
                return claims;
            }
            const offered = (request.headers.get('Sec-WebSocket-Protocol') ?? '').split(',').map(function (protocol) {
                return protocol.trim();
            });
            if (!offered.includes(ChangeBoardSubprotocol)) {
                return jsonResponse(400, { error: `offer the ${ChangeBoardSubprotocol} subprotocol beside the token` });
            }
            const headers = new Headers(request.headers);
            headers.set('Sec-WebSocket-Protocol', ChangeBoardSubprotocol);
            return changeBoardOf(environment).fetch(new Request('https://board/stream', { headers: headers }));
        }
        if (path === '/board/changes') {
            if (request.method !== 'GET') {
                return jsonResponse(405, { error: 'use GET' }, { Allow: 'GET' });
            }
            const claims = await authorize(request, environment, ChangeBoardName, { scopes: ['board'], queryScopes: [] });
            if (claims instanceof Response) {
                return claims;
            }
            return changeBoardOf(environment).fetch('https://board/changes');
        }
        if (path === '/') {
            return new Response('Loom pipeline. The endpoints are in docs/contracts.md.\n', {
                headers: { 'Content-Type': 'text/plain; charset=utf-8' },
            });
        }
        return jsonResponse(404, { error: 'no such endpoint' });
    },
} satisfies ExportedHandler<Env>;
