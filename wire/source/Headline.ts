// The headline on Loom's page (#lmboard): builds tested in the last hour and main's red count with its trend, read
// from the Queue's log. One Durable Object, named `headline`, follows the log through the Queue's binding from the
// last event it read, keeping only what the headline counts, so a viewer's read costs one small answer and the log is
// read once, a page at a time, never replayed per viewer. It reads at most once every few seconds however many viewers
// ask, and says how old its read is.
//
// A build tested is a future's whole decision, green or red: the judge's decision, or a whole verdict passed or failed
// from today's gate. A void tested nothing and is counted apart. Main's red count is the number of units main's last
// witness held red (main.red), zero once a witness of main's tip is green (main.green).

import { DurableObject } from 'cloudflare:workers';
import { queueOf } from './Changes';
import { jsonResponse } from './Http';
import { MaximumEventsPage, type QueueEvent } from './Queue';

export const HeadlineName = 'headline';
export const HeadlineFreshMilliseconds = 5000;
const hourMilliseconds = 60 * 60 * 1000;
const bucketMilliseconds = 5 * 60 * 1000;
// Decisions and main's reds older than this are dropped; the headline looks back an hour, the trend a day.
const keptMilliseconds = 7 * 24 * hourMilliseconds;
// A read follows at most this many pages of the log, so a long backlog is caught up over a few reads.
const maximumPagesPerRead = 20;

export type HeadlineOutcome = 'tested' | 'void';

export interface MainRedPoint {
    at: string;
    red: number;
    main: string;
}

export interface HeadlineReading {
    // When the log was last read, on the object's clock, and the last event read.
    readAt: string;
    seq: number;
    // Builds tested (green or red) in the hour before readAt, and each five minutes of it, oldest first.
    testedLastHour: number;
    buckets: number[];
    voidLastHour: number;
    // Main's red count now (null until a witness of main's tip is decided) and its points over the last day.
    mainRed: MainRedPoint | null;
    mainTrend: MainRedPoint[];
}

export function headlineOf(environment: Env): DurableObjectStub {
    const namespace = (environment as unknown as { Headline: DurableObjectNamespace }).Headline;
    return namespace.get(namespace.idFromName(HeadlineName));
}

// What one event adds to the headline, if anything: a whole decision's outcome, or main's red count.
export function headlineFact(event: QueueEvent): { outcome: HeadlineOutcome } | { red: number; main: string } | null {
    if (event.type === 'verdict.decided' && event.subject.unitKey === undefined) {
        const decision = event.data.decision as { status?: unknown } | undefined;
        const verdict = event.data.verdict as { status?: unknown } | undefined;
        const status = decision?.status ?? verdict?.status;
        if (status === 'green' || status === 'red' || status === 'passed' || status === 'failed') {
            return { outcome: 'tested' };
        }
        return status === 'void' ? { outcome: 'void' } : null;
    }
    if (event.type === 'main.red') {
        const units = event.data.units;
        return { red: Array.isArray(units) ? units.length : 0, main: String(event.data.main ?? '') };
    }
    if (event.type === 'main.green') {
        return { red: 0, main: String(event.data.main ?? '') };
    }
    return null;
}

export class Headline extends DurableObject<Env> {
    private readonly sql: SqlStorage;
    private lastReadAt = 0;
    private reading: Promise<void> | null = null;

    constructor(context: DurableObjectState, environment: Env) {
        super(context, environment);
        this.sql = context.storage.sql;
        this.sql.exec(`
            CREATE TABLE IF NOT EXISTS cursor (id INTEGER PRIMARY KEY CHECK (id = 1), seq INTEGER NOT NULL);
            CREATE TABLE IF NOT EXISTS decisions (seq INTEGER PRIMARY KEY, at INTEGER NOT NULL, outcome TEXT NOT NULL);
            CREATE TABLE IF NOT EXISTS mains (seq INTEGER PRIMARY KEY, at INTEGER NOT NULL, red INTEGER NOT NULL, main TEXT NOT NULL);
        `);
    }

    override async fetch(request: Request): Promise<Response> {
        if (new URL(request.url).pathname !== '/headline' || request.method !== 'GET') {
            return jsonResponse(404, { error: 'no such headline operation' });
        }
        if (Date.now() - this.lastReadAt >= HeadlineFreshMilliseconds) {
            // Concurrent viewers share one read of the log.
            this.reading ??= this.readLog().finally(() => {
                this.reading = null;
            });
            try {
                await this.reading;
            }
            catch (error) {
                return jsonResponse(502, { error: `the queue's log couldn't be read: ${error instanceof Error ? error.message : String(error)}` });
            }
        }
        return jsonResponse(200, this.headline(this.lastReadAt));
    }

    private cursor(): number {
        return this.sql.exec<{ seq: number }>('SELECT seq FROM cursor WHERE id = 1').toArray()[0]?.seq ?? 0;
    }

    private async readLog(): Promise<void> {
        const queue = queueOf(this.env);
        if (queue === null) {
            throw new Error("the queue isn't on the wire");
        }
        for (let page = 0; page < maximumPagesPerRead; page++) {
            const response = await queue.fetch(new Request(`https://queue/log?after=${this.cursor()}`));
            if (!response.ok) {
                await response.body?.cancel();
                throw new Error(`the log answered ${response.status}`);
            }
            const lines = new TextDecoder().decode(await response.arrayBuffer()).split('\n').filter(function (line) {
                return line !== '';
            });
            if (lines.length === 0) {
                break;
            }
            this.ctx.storage.transactionSync(() => {
                let seq = this.cursor();
                for (const line of lines) {
                    const event = JSON.parse(line) as QueueEvent;
                    const fact = headlineFact(event);
                    const at = Date.parse(event.at);
                    if (fact !== null && 'outcome' in fact) {
                        this.sql.exec('INSERT OR REPLACE INTO decisions (seq, at, outcome) VALUES (?, ?, ?)', event.seq, at, fact.outcome);
                    }
                    else if (fact !== null) {
                        this.sql.exec('INSERT OR REPLACE INTO mains (seq, at, red, main) VALUES (?, ?, ?, ?)', event.seq, at, fact.red, fact.main);
                    }
                    seq = event.seq;
                }
                this.sql.exec('INSERT OR REPLACE INTO cursor (id, seq) VALUES (1, ?)', seq);
            });
            if (lines.length < MaximumEventsPage) {
                break;
            }
        }
        const now = Date.now();
        this.sql.exec('DELETE FROM decisions WHERE at < ?', now - keptMilliseconds);
        // The newest main point is kept however old, since it is main's red count now.
        this.sql.exec('DELETE FROM mains WHERE at < ? AND seq < (SELECT MAX(seq) FROM mains)', now - keptMilliseconds);
        this.lastReadAt = now;
    }

    private headline(now: number): HeadlineReading {
        const since = now - hourMilliseconds;
        const buckets = new Array<number>(hourMilliseconds / bucketMilliseconds).fill(0);
        let voidLastHour = 0;
        for (const row of this.sql.exec<{ at: number; outcome: string }>('SELECT at, outcome FROM decisions WHERE at > ? AND at <= ?', since, now)) {
            if (row.outcome === 'void') {
                voidLastHour++;
                continue;
            }
            const index = Math.min(buckets.length - 1, Math.floor((row.at - since) / bucketMilliseconds));
            buckets[index] = (buckets[index] ?? 0) + 1;
        }
        const points = this.sql
            .exec<{ at: number; red: number; main: string }>('SELECT at, red, main FROM mains WHERE at > ? ORDER BY seq', now - 24 * hourMilliseconds)
            .toArray()
            .map(function (row) {
                return { at: new Date(row.at).toISOString(), red: row.red, main: row.main };
            });
        const newest = this.sql.exec<{ at: number; red: number; main: string }>('SELECT at, red, main FROM mains ORDER BY seq DESC LIMIT 1').toArray()[0];
        return {
            readAt: new Date(now).toISOString(),
            seq: this.cursor(),
            testedLastHour: buckets.reduce(function (sum, count) {
                return sum + count;
            }, 0),
            buckets: buckets,
            voidLastHour: voidLastHour,
            mainRed: newest === undefined ? null : { at: new Date(newest.at).toISOString(), red: newest.red, main: newest.main },
            mainTrend: points,
        };
    }
}
