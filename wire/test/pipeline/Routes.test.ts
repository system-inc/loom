// Every route loom answered before its UI moved in, and the status each answers with no token and with a board token,
// pinned so the UI's paths (the bare root and /ui/*) never change what the API answers. Reads and refused writes only:
// nothing here moves the queue.

import { describe, expect, it } from 'vitest';
import { call, token } from '../Helpers';

const change = 'chg_' + '0'.repeat(26);
const tree = 'f'.repeat(40);
const unitKey = 'a'.repeat(64);

const routes: [string, string][] = [
    ['GET', '/'],
    ['GET', '/board'],
    ['GET', '/board/'],
    ['POST', '/'],
    ['GET', '/board/changes'],
    ['GET', '/board/stream'],
    ['GET', '/favicon.svg'],
    ['POST', '/changes'],
    ['GET', '/changes/events'],
    ['GET', `/changes/${change}`],
    ['GET', `/changes/${change}/events`],
    ['GET', '/log'],
    ['GET', '/log?after=0'],
    ['GET', '/head'],
    ['GET', '/futures'],
    ['POST', `/futures/${tree}/plan`],
    ['POST', `/futures/${tree}/verdicts`],
    ['POST', `/futures/${tree}/unplan`],
    ['GET', `/verdicts/${unitKey}`],
    ['POST', '/verdicts'],
    ['GET', '/landings'],
    ['POST', `/landings/${change}`],
    ['GET', '/submissions'],
    ['POST', `/submissions/${change}/facts`],
    ['GET', '/blocks'],
    ['POST', '/blocks/1/built'],
    ['POST', '/rules'],
    ['GET', '/nothing'],
    ['GET', '/actions/list'],
];

async function statusOf(method: string, path: string, bearer: string | undefined): Promise<number> {
    const response = await call(path, { method: method, bearer: bearer, body: method === 'POST' ? '{}' : undefined });
    await response.body?.cancel();
    return response.status;
}

describe("loom's routes", function () {
    it('answer as they did before the UI moved in', async function () {
        const board = await token('board', 'board');
        const answers: string[] = [];
        for (const [method, path] of routes) {
            const none = await statusOf(method, path, undefined);
            const withBoard = method === 'GET' ? String(await statusOf(method, path, board)) : '-';
            answers.push(`${method} ${path}: ${none} ${withBoard}`);
        }
        expect(answers).toMatchInlineSnapshot(`
          [
            "GET /: 200 200",
            "GET /board: 200 200",
            "GET /board/: 200 200",
            "POST /: 405 -",
            "GET /board/changes: 401 200",
            "GET /board/stream: 401 401",
            "GET /favicon.svg: 200 200",
            "POST /changes: 401 -",
            "GET /changes/events: 401 403",
            "GET /changes/chg_00000000000000000000000000: 401 404",
            "GET /changes/chg_00000000000000000000000000/events: 401 404",
            "GET /log: 401 200",
            "GET /log?after=0: 401 200",
            "GET /head: 401 200",
            "GET /futures: 401 403",
            "POST /futures/ffffffffffffffffffffffffffffffffffffffff/plan: 401 -",
            "POST /futures/ffffffffffffffffffffffffffffffffffffffff/verdicts: 401 -",
            "POST /futures/ffffffffffffffffffffffffffffffffffffffff/unplan: 401 -",
            "GET /verdicts/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa: 401 403",
            "POST /verdicts: 401 -",
            "GET /landings: 401 403",
            "POST /landings/chg_00000000000000000000000000: 401 -",
            "GET /submissions: 401 403",
            "POST /submissions/chg_00000000000000000000000000/facts: 401 -",
            "GET /blocks: 401 403",
            "POST /blocks/1/built: 401 -",
            "POST /rules: 401 -",
            "GET /nothing: 404 404",
            "GET /actions/list: 404 404",
          ]
        `);
    });
});
