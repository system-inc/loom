// Shared test helpers: tokens signed with the pinned test secret, calls into the Worker, event lines and blobs.

import { exports } from 'cloudflare:workers';
import type { RunSummary, Snapshot } from '../source/Board';
import { mintToken, type TokenScope } from '../source/Token';

export const TestSecret = 'loom-test-secret';
export const Origin = 'https://wire.test';

const worker = (exports as unknown as { default: Fetcher }).default;

export function freshRun(): string {
    return 'r-' + crypto.randomUUID();
}

export function token(run: string, scope: TokenScope, expiresInSeconds = 3600): Promise<string> {
    return mintToken(TestSecret, { run: run, scope: scope, expires: Math.floor(Date.now() / 1000) + expiresInSeconds });
}

export function call(path: string, init: RequestInit & { bearer?: string } = {}): Promise<Response> {
    const headers = new Headers(init.headers);
    if (init.bearer !== undefined) {
        headers.set('Authorization', `Bearer ${init.bearer}`);
    }
    return worker.fetch(Origin + path, { ...init, headers: headers });
}

export function event(
    run: string,
    unit: string,
    sequence: number,
    type: string,
    fields: Record<string, unknown> = {},
): Record<string, unknown> {
    return { run: run, unit: unit, sequence: sequence, time: new Date(1791486000000 + sequence).toISOString(), type: type, ...fields };
}

// A unit's ordinary life: started, some output, exit, finished.
export function unitEvents(run: string, unit: string, status = 'passed', outputLines = 2): Record<string, unknown>[] {
    const events = [event(run, unit, 0, 'started', { machine: 'test-box', runnerVersion: '0.0.0', cpus: 2 })];
    for (let line = 0; line < outputLines; line++) {
        events.push(event(run, unit, events.length, 'output', { stream: 'stdout', text: `${unit} line ${line}` }));
    }
    events.push(event(run, unit, events.length, 'exit', { code: status === 'passed' ? 0 : 1, wallSeconds: 1.5 }));
    events.push(event(run, unit, events.length, 'finished', { status: status }));
    return events;
}

export function jsonLines(events: Record<string, unknown>[]): string {
    return events
        .map(function (value) {
            return JSON.stringify(value);
        })
        .join('\n');
}

export async function postEvents(run: string, bearer: string, events: Record<string, unknown>[]): Promise<Response> {
    return call(`/runs/${run}/events`, { method: 'POST', bearer: bearer, body: jsonLines(events) });
}

export async function postPlan(run: string, bearer: string, units: string[], inputs: string[] = []): Promise<Response> {
    return call(`/runs/${run}/plan`, { method: 'POST', bearer: bearer, body: JSON.stringify({ units: units, inputs: inputs }) });
}

export function verdictBody(status: string, failed: string[] = [], problems: string[] = [], cached: string[] = []): string {
    return JSON.stringify({ status: status, failed: failed, problems: problems, cached: cached });
}

export function postVerdict(run: string, bearer: string, body: string): Promise<Response> {
    return call(`/runs/${run}/verdict`, { method: 'POST', bearer: bearer, body: body });
}

export async function sha256Hex(bytes: Uint8Array): Promise<string> {
    const digest = await crypto.subtle.digest('SHA-256', bytes);
    return Array.from(new Uint8Array(digest), function (byte) {
        return byte.toString(16).padStart(2, '0');
    }).join('');
}

export function randomBytes(length: number): Uint8Array {
    const bytes = new Uint8Array(length);
    for (let offset = 0; offset < length; offset += 65536) {
        crypto.getRandomValues(bytes.subarray(offset, Math.min(length, offset + 65536)));
    }
    return bytes;
}

export function blobPath(run: string, sha256: string): string {
    return `/runs/${run}/blobs/${sha256}`;
}

// PUTs bytes through a run's blob endpoint and returns their hash with the answer.
export async function upload(run: string, bearer: string, bytes: Uint8Array): Promise<{ sha256: string; status: number }> {
    const sha256 = await sha256Hex(bytes);
    const response = await call(blobPath(run, sha256), { method: 'PUT', bearer: bearer, body: bytes });
    await response.body?.cancel();
    return { sha256: sha256, status: response.status };
}

export interface Viewer {
    frames: { kind: string; [field: string]: unknown }[];
    socket: WebSocket;
}

export async function openViewer(run: string, viewerToken: string): Promise<Viewer> {
    const response = await call(`/runs/${run}/stream?token=${encodeURIComponent(viewerToken)}`, {
        headers: { Upgrade: 'websocket' },
    });
    if (response.status !== 101 || response.webSocket === null) {
        throw new Error(`stream refused: ${response.status} ${await response.text()}`);
    }
    const socket = response.webSocket;
    const viewer: Viewer = { frames: [], socket: socket };
    socket.addEventListener('message', function (message) {
        viewer.frames.push(JSON.parse(String(message.data)));
    });
    socket.accept();
    return viewer;
}

export async function waitFor(condition: () => boolean, milliseconds = 3000): Promise<void> {
    const deadline = Date.now() + milliseconds;
    while (!condition()) {
        if (Date.now() > deadline) {
            throw new Error('timed out waiting');
        }
        await new Promise(function (resolve) {
            setTimeout(resolve, 10);
        });
    }
}

// A run id the way coordinator.RunId writes one: the job, the start time and four random bytes in hex.
export function datedRun(job: string): string {
    const suffix = Array.from(crypto.getRandomValues(new Uint8Array(4)), function (byte) {
        return byte.toString(16).padStart(2, '0');
    }).join('');
    return `${job}-20261008T200212-${suffix}`;
}

export function boardToken(expiresInSeconds = 3600): Promise<string> {
    return token('board', 'board', expiresInSeconds);
}

export async function boardSnapshot(bearer: string): Promise<Snapshot> {
    const response = await call('/board/snapshot', { bearer: bearer });
    if (response.status !== 200) {
        throw new Error(`snapshot refused: ${response.status} ${await response.text()}`);
    }
    return (await response.json()) as Snapshot;
}

// Polls the board's snapshot until the condition holds, since each run feeds the board from its alarm.
export async function snapshotWhen(bearer: string, condition: (snapshot: Snapshot) => boolean, milliseconds = 3000): Promise<Snapshot> {
    const deadline = Date.now() + milliseconds;
    for (;;) {
        const snapshot = await boardSnapshot(bearer);
        if (condition(snapshot)) {
            return snapshot;
        }
        if (Date.now() > deadline) {
            throw new Error(`timed out waiting for the board; it holds ${JSON.stringify(snapshot)}`);
        }
        await new Promise(function (resolve) {
            setTimeout(resolve, 20);
        });
    }
}

export function runOf(snapshot: Snapshot, run: string): RunSummary | undefined {
    return snapshot.runs.find(function (summary) {
        return summary.run === run;
    });
}

// Waits until the board holds the run and the condition holds for its summary, and returns the summary.
export async function boardRunWhen(bearer: string, run: string, condition: (summary: RunSummary) => boolean): Promise<RunSummary> {
    const snapshot = await snapshotWhen(bearer, function (candidate) {
        const summary = runOf(candidate, run);
        return summary !== undefined && condition(summary);
    });
    return runOf(snapshot, run) as RunSummary;
}

// Opens the board's stream offering these subprotocols, the way the page does: "loom, token.<token>".
export function boardStream(subprotocols: string | null, init: RequestInit & { bearer?: string } = {}): Promise<Response> {
    const headers = new Headers(init.headers);
    headers.set('Upgrade', 'websocket');
    if (subprotocols !== null) {
        headers.set('Sec-WebSocket-Protocol', subprotocols);
    }
    return call('/board/stream', { ...init, headers: headers });
}

export async function openBoard(bearer: string): Promise<Viewer & { response: Response }> {
    const response = await boardStream(`loom, token.${bearer}`);
    if (response.status !== 101 || response.webSocket === null) {
        throw new Error(`board stream refused: ${response.status} ${await response.text()}`);
    }
    const socket = response.webSocket;
    const viewer = { frames: [] as Viewer['frames'], socket: socket, response: response };
    socket.addEventListener('message', function (message) {
        viewer.frames.push(JSON.parse(String(message.data)));
    });
    socket.accept();
    return viewer;
}
