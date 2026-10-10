// The page's headline: builds tested in the last hour and main's red count, followed from the Queue's log through its
// binding and read with a board token. The log here is a real one (OldQueueLog.ts), moved into the last hour.

import { env } from 'cloudflare:workers';
import { runInDurableObject } from 'cloudflare:test';
import { afterEach, describe, expect, it } from 'vitest';
import { QueueName } from '../../source/Changes';
import { headlineFact, type HeadlineReading } from '../../source/Headline';
import type { QueueEvent } from '../../source/Queue';
import { call, token } from '../Helpers';
import { oldQueueLog } from './OldQueueLog';

const queues = (env as unknown as { Queue: DurableObjectNamespace }).Queue;
const headlines = (env as unknown as { Headline: DurableObjectNamespace }).Headline;

// Writes events straight into the Queue object's log, as its own appends would have, so /log serves them. A row given
// as [seq, text] is stored as that text, whatever it is. Storage is shared between tests here, so each test's rows are
// taken out again after it, and the headline forgets what it read.
const seeded: number[] = [];

async function seed(events: (QueueEvent | [number, string])[]): Promise<void> {
    const queue = queues.get(queues.idFromName(QueueName));
    await runInDurableObject(queue, function (_instance: unknown, state: DurableObjectState) {
        for (const event of events) {
            const row = Array.isArray(event)
                ? { seq: event[0], json: event[1], type: 'change.submitted', change: null }
                : { seq: event.seq, json: JSON.stringify(event), type: event.type, change: event.subject.change ?? null };
            state.storage.sql.exec('INSERT INTO events (seq, json, hash, type, change) VALUES (?, ?, ?, ?, ?)', row.seq, row.json, 'h'.repeat(64), row.type, row.change);
            seeded.push(row.seq);
        }
    });
}

afterEach(async function () {
    const rows = seeded.splice(0);
    await runInDurableObject(queues.get(queues.idFromName(QueueName)), function (_instance: unknown, state: DurableObjectState) {
        for (const seq of rows) {
            state.storage.sql.exec('DELETE FROM events WHERE seq = ?', seq);
        }
    });
    await runInDurableObject(headlines.get(headlines.idFromName('headline')), function (instance: unknown, state: DurableObjectState) {
        state.storage.sql.exec('DELETE FROM cursor; DELETE FROM decisions; DELETE FROM mains;');
        (instance as { lastReadAt: number }).lastReadAt = 0;
    });
});

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
        // A board token goes in the Authorization header, never the address, where a server would log it.
        const inQuery = await call(`/ui/headline?token=${board}`);
        expect(inQuery.status).toBe(401);
        const response = await readHeadline(board);
        expect(response.status).toBe(200);
        const reading = (await response.json()) as HeadlineReading;
        expect(reading.seq).toBe(21);
        expect(reading.testedLastHour).toBe(3);
        expect(reading.voidLastHour).toBe(1);
        expect(reading.buckets).toHaveLength(12);
        expect(reading.buckets[11]).toBe(3);
        expect(reading.mainRed?.red).toBe(0);
        expect(reading.unreadable).toBeNull();
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

    it('skips a log line it can\'t read, names it, and reads on past it', async function () {
        const at = new Date().toISOString();
        await seed([
            ...recentLog(),
            [22, 'not a json line {'],
            [23, JSON.stringify({ seq: 'twenty-three', at: at })],
            { seq: 24, at: at, prev: '', type: 'main.red', subject: { change: 'chg_x' }, data: { main: 'c'.repeat(40), units: ['a', 'b'], witness: 'chg_x' } },
        ]);
        const response = await readHeadline(await token('board', 'board'));
        expect(response.status).toBe(200);
        const reading = (await response.json()) as HeadlineReading;
        expect(reading.seq).toBe(24);
        expect(reading.unreadable).toEqual({ lines: 2, afterSeq: 21 });
        expect(reading.testedLastHour).toBe(3);
        expect(reading.mainRed?.red).toBe(2);
    });
});
