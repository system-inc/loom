// loom-pipeline's front door (docs/contracts.md, section 5): an owner submits a change with their submit token,
// anyone with a reason reads one, and the Mac's bridge reads what owners act on. The tokens are checked by the same
// verifier as loom-wire's; Changes.ts checks the rest and asks the Queue object.

import { handleAction } from './Actions';
import { handleChanges, queueOf } from './Changes';
import { jsonResponse } from './Http';
import type { TokenScope } from './Token';
import { authorize } from './Worker';

// A change is submitted with its owner's submit token; any of these reads one, and Changes.ts says which may do what.
const changeScopes: TokenScope[] = ['submit', 'coordinator', 'board'];

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
        // The action store checks its own build token, since no other scope may reach it.
        const action = await handleAction(request, environment);
        if (action !== null) {
            return action;
        }
        if (path === '/') {
            return new Response('Loom pipeline. The endpoints are in docs/contracts.md.\n', {
                headers: { 'Content-Type': 'text/plain; charset=utf-8' },
            });
        }
        return jsonResponse(404, { error: 'no such endpoint' });
    },
} satisfies ExportedHandler<Env>;
