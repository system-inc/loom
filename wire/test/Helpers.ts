// Shared test helpers: tokens signed with the pinned test secret, calls into the Worker, and event lines.

import { exports } from 'cloudflare:workers';
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

export async function postPlan(run: string, bearer: string, units: string[]): Promise<Response> {
    return call(`/runs/${run}/plan`, { method: 'POST', bearer: bearer, body: JSON.stringify({ units: units }) });
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
