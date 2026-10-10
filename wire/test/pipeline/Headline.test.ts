// The page's headline: builds tested in the last hour and main's red count, followed from the Queue's log through its
// binding and read with a board token. The log here is a real one (OldQueueLog.ts), moved into the last hour.

import { env } from 'cloudflare:workers';
import { runInDurableObject } from 'cloudflare:test';
import { describe, expect, it } from 'vitest';
import { QueueName } from '../../source/Changes';
import { headlineFact, type HeadlineReading } from '../../source/Headline';
import type { QueueEvent } from '../../source/Queue';
import { call, token } from '../Helpers';
import { oldQueueLog } from './OldQueueLog';

const queues = (env as unknown as { Queue: DurableObjectNamespace }).Queue;
const headlines = (env as unknown as { Headline: DurableObjectNamespace }).Headline;

// Writes events straight into the Queue object's log, as its own appends would have, so /log serves them.
async function seed(events: QueueEvent[]): Promise<void> {
    const queue = queues.get(queues.idFromName(QueueName));
    await runInDurableObject(queue, function (_instance: unknown, state: DurableObjectState) {
        for (const event of events) {
            state.storage.sql.exec(
                'INSERT INTO events (seq, json, hash, type, change) VALUES (?, ?, ?, ?, ?)',
                event.seq,
                JSON.stringify(event),
                'h'.repeat(64),
                event.type,
                event.subject.change ?? null,
            );
        }
    });
}

// The log, its times moved so its last event is a minute ago, a second apart.
function recentLog(): QueueEvent[] {
    const last = Date.now() - 60 * 1000;
    return oldQueueLog.log.map(function (event, index, log) {
        return { ...event, at: new Date(last - (log.length - 1 - index) * 1000).toISOString() };
    });
}

async function readHeadline(bearer?: string): Promise<Response> {
    return call('/ui/headline', { bearer: bearer });
}

describe('the headline', function () {
    it('counts a whole decision, green or red, as a build tested, a void apart, and main red by its units', function () {
        const facts = oldQueueLog.log.flatMap(function (event) {
            const fact = headlineFact(event);
            return fact === null ? [] : [`${event.seq} ${JSON.stringify(fact)}`];
        });
        expect(facts).toEqual([
            '5 {"outcome":"tested"}',
            `7 {"red":1,"main":"${'0'.repeat(38)}e7"}`,
            '12 {"outcome":"void"}',
            '14 {"outcome":"tested"}',
            `15 {"red":0,"main":"${'0'.repeat(38)}e7"}`,
            '20 {"outcome":"tested"}',
        ]);
    });

    it('reads the log once, a page at a time, and answers a board token only', async function () {
        await seed(recentLog());
        expect((await readHeadline()).status).toBe(401);
        expect((await readHeadline(await token('board', 'submit'))).status).toBe(403);
        const board = await token('board', 'board');
        const response = await readHeadline(board);
        expect(response.status).toBe(200);
        const reading = (await response.json()) as HeadlineReading;
        expect(reading.seq).toBe(21);
        expect(reading.testedLastHour).toBe(3);
        expect(reading.voidLastHour).toBe(1);
        expect(reading.buckets).toHaveLength(12);
        expect(reading.buckets[11]).toBe(3);
        expect(reading.mainRed?.red).toBe(0);
        expect(
            reading.mainTrend.map(function (point) {
                return point.red;
            }),
        ).toEqual([1, 0]);

        // A new main.red: a read within a few seconds answers from what was read; a later one follows the log on.
        const at = new Date().toISOString();
        await seed([{ seq: 22, at: at, prev: '', type: 'main.red', subject: { change: 'chg_x' }, data: { main: 'b'.repeat(40), units: ['a', 'b'], witness: 'chg_x' } }]);
        const cached = (await (await readHeadline(board)).json()) as HeadlineReading;
        expect(cached.seq).toBe(21);
        await runInDurableObject(headlines.get(headlines.idFromName('headline')), function (instance: unknown) {
            (instance as { lastReadAt: number }).lastReadAt = 0;
        });
        const later = (await (await readHeadline(board)).json()) as HeadlineReading;
        expect(later.seq).toBe(22);
        expect(later.mainRed).toEqual({ at: at, red: 2, main: 'b'.repeat(40) });
        expect(later.testedLastHour).toBe(3);
    });
});
