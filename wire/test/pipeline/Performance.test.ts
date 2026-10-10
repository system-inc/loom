// Loom's performance: the stage timeline of the verify of main (chg_2bfxkyyp), built from the Queue's real log and run
// future-ebdb6c53…-4's real events (VerifyOfMain.ts), followed the way the deployed object follows them, and read with
// a board token. Every expected time below is one the log or the run's events stamped.

import { env } from 'cloudflare:workers';
import { runInDurableObject } from 'cloudflare:test';
import { afterEach, describe, expect, it } from 'vitest';
import { QueueName } from '../../source/Changes';
import { attemptOf, percentile, type CandidateTimeline, type Performance } from '../../source/Performance';
import { canonical, GenesisHash, sha256Text, type QueueEvent } from '../../source/Queue';
import { verifyToken } from '../../source/Token';
import { call, TestSecret, token } from '../Helpers';
import { verifyCandidate, verifyLog, verifyRun, verifyRunEvents } from './VerifyOfMain';

const queues = (env as unknown as { Queue: DurableObjectNamespace }).Queue;
const performances = (env as unknown as { Performance: DurableObjectNamespace }).Performance;

// The log's data was cut to keep the fixture small, so it is chained again here, as the Queue's appends chain it.
async function seedQueue(events: QueueEvent[]): Promise<void> {
    const rows: { seq: number; json: string; hash: string; type: string; change: string | null }[] = [];
    let prev = GenesisHash;
    for (const event of events) {
        const chained = { ...event, prev: prev };
        const json = canonical(chained);
        prev = await sha256Text(json);
        rows.push({ seq: event.seq, json: json, hash: prev, type: event.type, change: event.subject.change ?? null });
    }
    await runInDurableObject(queues.get(queues.idFromName(QueueName)), function (instance: unknown, state: DurableObjectState) {
        for (const row of rows) {
            state.storage.sql.exec('INSERT INTO events (seq, json, hash, type, change) VALUES (?, ?, ?, ?, ?)', row.seq, row.json, row.hash, row.type, row.change);
        }
        // The Queue keeps its state in memory once read; it replays the seeded log on its next read.
        (instance as { state: unknown }).state = null;
    });
}

// The runs Worker as the object sees it: each run's events after a position, a page at a time, and the token each read
// carried. A run it holds nothing for answers at once with nothing, as the real one does after its wait.
function fakeRuns(events: Record<string, { position: number; event: Record<string, unknown> }[]>, pageSize = 1000) {
    const reads: { run: string; after: number; scope: string; tokenRun: string }[] = [];
    const fetcher = {
        async fetch(input: RequestInfo | URL, init?: RequestInit): Promise<Response> {
            const url = new URL(String(input));
            const match = /^\/runs\/([^/]+)\/events$/.exec(url.pathname);
            const run = decodeURIComponent(match?.[1] ?? '');
            const bearer = new Headers(init?.headers).get('Authorization')?.replace(/^Bearer /, '') ?? '';
            const verification = await verifyToken(TestSecret, bearer, Math.floor(Date.now() / 1000));
            const after = Number(url.searchParams.get('after') ?? '0');
            reads.push({ run: run, after: after, scope: verification.valid ? verification.claims.scope : 'invalid', tokenRun: verification.valid ? verification.claims.run : '' });
            const lines = (events[run] ?? [])
                .filter((row) => row.position > after)
                .slice(0, pageSize)
                .map((row) => JSON.stringify(row) + '\n');
            return new Response(lines.join(''), { headers: { 'Content-Type': 'application/x-ndjson' } });
        },
    };
    return { fetcher: fetcher as unknown as Fetcher, reads: reads };
}

async function followWith(runs: Fetcher, pageSize = 1000): Promise<void> {
    await runInDurableObject(performances.get(performances.idFromName('performance')), async function (instance: unknown) {
        const performance = instance as Performance;
        performance.runs = runs;
        performance.runEventsPage = pageSize;
        // A read arms the alarm at once, so a round of its own may be under way: the first call joins it, and the
        // second is a round of this test's own.
        await performance.follow();
        await performance.follow();
    });
}

async function readTimeline(candidate: string): Promise<{ status: number; body: { timeline?: CandidateTimeline; seq?: number; readAt?: string | null; error?: string } }> {
    const response = await call(`/performance/candidates/${candidate}`, { bearer: await token('board', 'board') });
    return { status: response.status, body: await response.json() };
}

afterEach(async function () {
    await runInDurableObject(queues.get(queues.idFromName(QueueName)), function (instance: unknown, state: DurableObjectState) {
        state.storage.sql.exec('DELETE FROM events');
        (instance as { state: unknown }).state = null;
    });
    await runInDurableObject(performances.get(performances.idFromName('performance')), async function (_instance: unknown, state: DurableObjectState) {
        state.storage.sql.exec('DELETE FROM cursor; DELETE FROM branches; DELETE FROM candidates; DELETE FROM plans; DELETE FROM replans; DELETE FROM planned; DELETE FROM runs; DELETE FROM units;');
        await state.storage.deleteAlarm();
    });
});

describe("the verify of main's timeline", function () {
    it('is read with a board token only', async function () {
        expect((await call(`/performance/candidates/${verifyCandidate}`)).status).toBe(401);
        expect((await call(`/performance/candidates/${verifyCandidate}`, { bearer: await token('board', 'submit') })).status).toBe(403);
        expect((await call(`/performance/candidates/${verifyCandidate}`, { method: 'POST', bearer: await token('board', 'board') })).status).toBe(405);
        expect((await readTimeline(verifyCandidate)).status).toBe(404);
    });

    it('follows the log and run 4 by rule: every stage from the log and the run, and the time nothing accounts for named', async function () {
        await seedQueue(verifyLog);
        const runs = fakeRuns({ [verifyRun]: verifyRunEvents });
        await followWith(runs.fetcher);

        // The Queue's own planned listing named attempt 4; runs 1 and 2 came from their verdicts.
        expect([...new Set(runs.reads.map((read) => read.run))].sort()).toEqual([verifyRun.replace(/-4$/, '-1'), verifyRun.replace(/-4$/, '-2'), verifyRun]);
        // Each read carried a coordinator token for its own run, and nothing else.
        for (const read of runs.reads) {
            expect(read.scope).toBe('coordinator');
            expect(read.tokenRun).toBe(read.run);
        }

        const read = await readTimeline(verifyCandidate);
        expect(read.status).toBe(200);
        expect(read.body.seq).toBe(277);
        const timeline = read.body.timeline!;
        expect(timeline.branch).toBe('chg_2bfxkyyp0pqjkrxxw8x9w3d08w');
        expect(timeline.witness).toBe(true);
        expect(timeline.runs.map((run) => [attemptOf(run.run), run.status])).toEqual([
            [1, 'void'],
            [2, 'void'],
            [4, null],
        ]);
        const shape = function (index: number): string[] {
            return timeline.runs[index]!.stages.map(function (stage) {
                return `${stage.stage} ${stage.start.slice(11, 23)} ${stage.end?.slice(11, 23) ?? 'open'}`;
            });
        };
        // Run 1: submit, the bridge's facts 42 s later, the planner's 68 units 5 min later, and a void two hours after,
        // with no unit ever started: the plan named a runner no pool could place.
        expect(shape(0)).toEqual(['admission 17:43:55.233 17:44:37.518', 'planning 17:44:37.518 17:49:38.647', 'judge 17:49:38.647 19:51:38.837']);
        expect(timeline.runs[0]!.stages[2]!.note).toBe('no unit started');
        // Run 2: voided 31 s after run 1, on the same plan.
        expect(shape(1)).toEqual(['judge 19:51:38.837 19:52:10.178']);
        // Run 4: ten minutes until the plan was withdrawn, the new plan, the tree and placement, the tests, and the judge
        // still open: all 68 units finished at 21:01:08 and no verdict is in the log.
        expect(shape(2)).toEqual([
            'wait 19:52:10.178 20:02:21.415',
            'planning 20:02:21.415 20:19:40.122',
            'tree 20:19:40.122 20:38:54.244',
            'tests 20:38:54.244 21:01:08.023',
            'judge 21:01:08.023 open',
        ]);
        const run4 = timeline.runs[2]!;
        expect(run4.units).toEqual({ planned: 68, reused: 0, started: 68, ready: 60, finished: 68, passed: 26, failed: 7, broken: 35 });
        expect(run4.stages[2]!.seconds).toBeCloseTo(1154.122, 3);
        expect(run4.stages[3]!.seconds).toBeCloseTo(1333.779, 3);
        expect(run4.stages[4]!.open).toBe(true);
        // Each unit's ready, from the runner's own line: what a box took to fetch and unpack. Eight never said ready.
        expect(run4.ready).toEqual({ units: 60, p50: 3, max: 71.6 });
    });

    it('reads a long run a page at a time, and comes back for what arrived since', async function () {
        await seedQueue(verifyLog);
        const half = Math.floor(verifyRunEvents.length / 2);
        const first = fakeRuns({ [verifyRun]: verifyRunEvents.slice(0, half) }, 100);
        await followWith(first.fetcher, 100);
        const pages = first.reads.filter((read) => read.run === verifyRun);
        // Two pages, the second short, then the next round's one read after where it stopped.
        expect(half).toBe(183);
        expect(pages.map((read) => read.after)).toEqual([0, verifyRunEvents[99]!.position, verifyRunEvents[half - 1]!.position]);
        const second = fakeRuns({ [verifyRun]: verifyRunEvents }, 100);
        await followWith(second.fetcher, 100);
        // It asks after the last position it read, never from the start again.
        expect(second.reads.find((read) => read.run === verifyRun)?.after).toBe(verifyRunEvents[half - 1]!.position);
        expect((await readTimeline(verifyCandidate)).body.timeline!.runs[2]!.units.finished).toBe(68);
    });

    it('closes a decided run once its events are read through, and never asks it again', async function () {
        await seedQueue(verifyLog);
        await followWith(fakeRuns({ [verifyRun]: verifyRunEvents }).fetcher);
        const again = fakeRuns({ [verifyRun]: verifyRunEvents });
        await followWith(again.fetcher);
        expect(new Set(again.reads.map((read) => read.run))).toEqual(new Set([verifyRun]));
    });

    it('lists a day by candidate, each run with its stages, and a branch narrows it', async function () {
        await seedQueue(verifyLog);
        await followWith(fakeRuns({ [verifyRun]: verifyRunEvents }).fetcher);
        const board = await token('board', 'board');
        const day = (await (await call('/performance/candidates?day=2026-10-10', { bearer: board })).json()) as { candidates: { candidate: string; runs: { stages: { stage: string }[] }[] }[] };
        expect(day.candidates.map((candidate) => candidate.candidate)).toEqual([verifyCandidate]);
        expect(day.candidates[0]!.runs[2]!.stages.map((stage) => stage.stage)).toEqual(['wait', 'planning', 'tree', 'tests', 'judge']);
        const other = (await (await call('/performance/candidates?day=2026-10-10&branch=chg_none', { bearer: board })).json()) as { candidates: unknown[] };
        expect(other.candidates).toEqual([]);
        expect((await call('/performance/candidates?day=yesterday', { bearer: board })).status).toBe(400);
    });

    it('takes the nearest rank, so a percentile is a figure some unit really had', function () {
        expect(percentile([3, 1, 2], 0.5)).toBe(2);
        expect(percentile([4, 1, 3, 2], 0.5)).toBe(2);
        expect(percentile([5], 0.9)).toBe(5);
    });
});
