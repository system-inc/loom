import { describe, expect, it } from 'vitest';
import { pulseOf, type RunSummary } from '../source/Board';
import { boardToken, event, freshRun, postEvents, postPlan, postVerdict, runOf, snapshotWhen, token, verdictBody } from './Helpers';

// Its own file, so its own storage: the board here holds only these runs, and the pulse's numbers are exact.
describe("the board's pulse", function () {
    it('counts units running, finished in the last minute and queued, and names the longest running', async function () {
        const board = await boardToken();
        const live = freshRun();
        const over = freshRun();
        const liveRunner = await token(live, 'runner');
        const overRunner = await token(over, 'runner');
        await postPlan(live, await token(live, 'coordinator'), ['a1', 'a2', 'a3', 'a4', 'a5']);
        await postPlan(over, await token(over, 'coordinator'), ['b1', 'b2']);
        await postEvents(live, liveRunner, [
            event(live, 'a1', 0, 'started', { machine: 'home', time: '2026-10-08T10:00:00Z' }),
            // Half a second after a1, though it sorts first as text: the longest is decided by time.
            event(live, 'a2', 0, 'started', { machine: 'home', time: '2026-10-08T10:00:00.5Z' }),
            event(live, 'a3', 0, 'started', { machine: 'away', time: '2026-10-08T09:30:00Z' }),
            event(live, 'a3', 1, 'finished', { status: 'passed' }),
            // Served from the cache: finished, never running.
            event(live, 'a5', 0, 'cached', { key: 'a'.repeat(64), fromRun: 'r-earlier', events: 'b'.repeat(64) }),
            event(live, 'a5', 1, 'finished', { status: 'passed' }),
        ]);
        // A run with its verdict: its unit still listed as running started earliest, yet counts nowhere.
        await postEvents(over, overRunner, [
            event(over, 'b1', 0, 'started', { machine: 'away', time: '2026-10-08T09:00:00Z' }),
            event(over, 'b1', 1, 'finished', { status: 'failed' }),
            event(over, 'b2', 0, 'started', { machine: 'away', time: '2026-10-08T08:00:00Z' }),
        ]);
        await postVerdict(over, await token(over, 'coordinator'), verdictBody('void', ['b1'], ['b2 never finished']));

        const snapshot = await snapshotWhen(board, function (candidate) {
            return runOf(candidate, live)?.passed === 2 && runOf(candidate, over)?.verdict === 'void';
        });
        expect(snapshot.runs).toHaveLength(2);
        expect(runOf(snapshot, live)).toMatchObject({ queued: 1, running: 2, passed: 2, cached: 1 });
        expect(snapshot.pulse).toEqual({
            running: 2,
            finishedLastMinute: 3,
            queued: 1,
            longest: { unit: 'a1', run: live, machine: 'home', since: '2026-10-08T10:00:00Z' },
        });
    });

    it('is null for the longest when nothing runs', function () {
        expect(pulseOf([], 0)).toEqual({ running: 0, finishedLastMinute: 0, queued: 0, longest: null });
        const queuedOnly = { run: 'r', verdict: null, running: 0, queued: 4, active: [] } as unknown as RunSummary;
        expect(pulseOf([queuedOnly], 0)).toEqual({ running: 0, finishedLastMinute: 0, queued: 4, longest: null });
    });
});
