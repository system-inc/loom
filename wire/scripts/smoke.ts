// Proves the deployed wire end to end with the real secret. Run from wire/: node scripts/smoke.ts [baseUrl]
//
// 1. A fresh run: the coordinator asks for an input (HEAD), uploads it, and sets a plan of 3 units naming it;
//    events posted out of order, with one replay; a viewer watching live and a viewer joining late must each
//    see every event exactly once, in order, then the verdict.
// 2. Blobs through the run: put, put again (left as is), get, a bad hash refused, the Store table's refusals
//    (a runner reading outside its plan and uploads, a viewer writing), a viewer reading by ?token=.
// 3. The cache: an entry naming the run's blobs written once, refused when its blobs aren't held, read back.
// 4. The verdict, then runs/<run>/events.jsonl and runs/<run>/blobs.jsonl read back from R2 with wrangler.
// 5. Latency, POST start to WebSocket receipt: 200 events one at a time, then 50 units writing at once.

import { execFileSync } from 'node:child_process';
import { createHash, randomBytes } from 'node:crypto';
import { readFileSync } from 'node:fs';
import { homedir } from 'node:os';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';
import { mintToken, type TokenScope } from '../source/Token.ts';

const accountId = '8863664b1d7b406f63020b58b45764c5';
const baseUrl = (process.argv[2] ?? 'https://loom-wire.kirk-ouimet.workers.dev').replace(/\/$/, '');
const wireDirectory = join(dirname(fileURLToPath(import.meta.url)), '..');
const secret = readFileSync(join(homedir(), '.loom', 'token-secret'), 'utf8').trim();
const daySeconds = 24 * 60 * 60;

type Frame = { kind: string; position?: number; event?: Record<string, unknown>; [field: string]: unknown };

let failures = 0;
function check(condition: boolean, description: string): void {
    console.log(`${condition ? 'ok  ' : 'FAIL'} ${description}`);
    if (!condition) {
        failures++;
    }
}

function tokenFor(run: string, scope: TokenScope, seconds: number): Promise<string> {
    return mintToken(secret, { run: run, scope: scope, expires: Math.floor(Date.now() / 1000) + seconds });
}

async function request(path: string, bearer: string | null, init: RequestInit = {}): Promise<Response> {
    const headers = new Headers(init.headers);
    if (bearer !== null) {
        headers.set('Authorization', `Bearer ${bearer}`);
    }
    return fetch(baseUrl + path, { ...init, headers: headers });
}

function event(run: string, unit: string, sequence: number, type: string, fields: Record<string, unknown> = {}) {
    return { run: run, unit: unit, sequence: sequence, time: new Date().toISOString(), type: type, ...fields };
}

function unitStory(run: string, unit: string, status: 'passed' | 'failed', outputLines: number) {
    const story: Record<string, unknown>[] = [event(run, unit, 0, 'started', { machine: 'smoke', runnerVersion: 'smoke', cpus: 1 })];
    for (let line = 0; line < outputLines; line++) {
        const stream = status === 'failed' && line % 5 === 4 ? 'stderr' : 'stdout';
        story.push(event(run, unit, story.length, 'output', { stream: stream, text: `${unit}: line ${line + 1} of ${outputLines}` }));
    }
    if (status === 'failed') {
        story.push(event(run, unit, story.length, 'output', { stream: 'stderr', text: `${unit}: assertion failed, expected 4 got 5` }));
    }
    story.push(event(run, unit, story.length, 'exit', { code: status === 'passed' ? 0 : 1, wallSeconds: 2.25, userSeconds: 1.5, systemSeconds: 0.25 }));
    story.push(event(run, unit, story.length, 'finished', { status: status }));
    return story;
}

function jsonLines(events: Record<string, unknown>[]): string {
    return events.map((value) => JSON.stringify(value)).join('\n') + '\n';
}

interface Viewer {
    frames: Frame[];
    socket: WebSocket;
    received: Map<string, number>; // "unit sequence" to arrival time
}

function openViewer(run: string, viewerToken: string): Promise<Viewer> {
    const url = baseUrl.replace(/^http/, 'ws') + `/runs/${run}/stream?token=${encodeURIComponent(viewerToken)}`;
    const socket = new WebSocket(url);
    const viewer: Viewer = { frames: [], socket: socket, received: new Map() };
    socket.addEventListener('message', function (message) {
        const arrived = performance.now();
        const frame = JSON.parse(String(message.data)) as Frame;
        viewer.frames.push(frame);
        if (frame.kind === 'event' && frame.event !== undefined) {
            viewer.received.set(`${frame.event.unit} ${frame.event.sequence}`, arrived);
        }
    });
    return new Promise(function (resolve, reject) {
        socket.addEventListener('open', () => resolve(viewer));
        socket.addEventListener('error', () => reject(new Error(`WebSocket to ${run} failed`)));
    });
}

async function waitFor(condition: () => boolean, description: string, milliseconds = 20000): Promise<void> {
    const deadline = Date.now() + milliseconds;
    while (!condition()) {
        if (Date.now() > deadline) {
            throw new Error(`timed out waiting for ${description}`);
        }
        await new Promise((resolve) => setTimeout(resolve, 5));
    }
}

function hasFrame(viewer: Viewer, kind: string): boolean {
    return viewer.frames.some((frame) => frame.kind === kind);
}

// Every event exactly once, each unit's sequence 0, 1, 2 ... in order, positions climbing, verdict last.
function storyIsWhole(viewer: Viewer, expected: Record<string, unknown>[]): boolean {
    const logged = viewer.frames.filter((frame) => frame.kind === 'event');
    const seen = new Set<string>();
    const next = new Map<string, number>();
    let lastPosition = 0;
    for (const frame of logged) {
        const unit = String(frame.event?.unit);
        const sequence = Number(frame.event?.sequence);
        const key = `${unit} ${sequence}`;
        if (seen.has(key) || sequence !== (next.get(unit) ?? 0) || (frame.position ?? 0) <= lastPosition) {
            return false;
        }
        seen.add(key);
        next.set(unit, sequence + 1);
        lastPosition = frame.position ?? 0;
    }
    return logged.length === expected.length;
}

function sha256Hex(bytes: Uint8Array): string {
    return createHash('sha256').update(bytes).digest('hex');
}

function summary(values: number[]): { median: number; p95: number; max: number; min: number; count: number } {
    const sorted = [...values].sort((left, right) => left - right);
    const at = (fraction: number) => sorted[Math.min(sorted.length - 1, Math.floor(fraction * sorted.length))] ?? NaN;
    const middle = sorted.length / 2;
    const median = sorted.length % 2 === 0 ? ((sorted[middle - 1] ?? 0) + (sorted[middle] ?? 0)) / 2 : (sorted[Math.floor(middle)] ?? NaN);
    const round = (value: number) => Math.round(value * 10) / 10;
    return { median: round(median), p95: round(at(0.95)), max: round(sorted.at(-1) ?? NaN), min: round(sorted[0] ?? NaN), count: sorted.length };
}

async function smokeRun(): Promise<string> {
    const run = `smoke-${new Date().toISOString().replace(/[-:]/g, '').replace(/\..*$/, '')}-${randomBytes(3).toString('hex')}`;
    console.log(`\nrun ${run}`);
    const coordinator = await tokenFor(run, 'coordinator', daySeconds);
    const runner = await tokenFor(run, 'runner', daySeconds);
    const viewerToken = await tokenFor(run, 'viewer', 7 * daySeconds);
    const units = ['build', 'tests-shard-0', 'tests-shard-1'];

    const live = await openViewer(run, viewerToken);
    await waitFor(() => hasFrame(live, 'caughtUp'), 'the live viewer to catch up');

    // The coordinator asks before uploading an input, so one already held costs no bytes.
    const input = new Uint8Array(randomBytes(4096));
    const inputHash = sha256Hex(input);
    const inputPath = `/runs/${run}/blobs/${inputHash}`;
    const inputAsked = await request(inputPath, coordinator, { method: 'HEAD' });
    check(inputAsked.status === 404, `HEAD of a new input says it isn't held (${inputAsked.status})`);
    const inputPut = await request(inputPath, coordinator, { method: 'PUT', body: input });
    check(inputPut.status === 201, `input PUT by the coordinator stored (${inputPut.status})`);
    const inputHeld = await request(inputPath, coordinator, { method: 'HEAD' });
    check(inputHeld.status === 200 && inputHeld.headers.get('Content-Length') === String(input.length), `HEAD of the input now says ${inputHeld.headers.get('Content-Length')} bytes`);

    const plan = { units: units, inputs: [inputHash] };
    const planResponse = await request(`/runs/${run}/plan`, coordinator, { method: 'POST', body: JSON.stringify(plan) });
    check(planResponse.status === 201, `plan set (${planResponse.status})`);
    const samePlan = await request(`/runs/${run}/plan`, coordinator, { method: 'POST', body: JSON.stringify(plan) });
    check(samePlan.status === 200, `the same plan again is fine (${samePlan.status})`);
    const differentPlan = await request(`/runs/${run}/plan`, coordinator, { method: 'POST', body: JSON.stringify({ units: units, inputs: [] }) });
    check(differentPlan.status === 409, `a plan with different inputs refused (${differentPlan.status})`);
    const v1Plan = await request(`/runs/${run}/plan`, coordinator, { method: 'POST', body: JSON.stringify({ units: units }) });
    check(v1Plan.status === 400, `a plan without inputs refused (${v1Plan.status})`);

    const build = unitStory(run, 'build', 'passed', 3);
    const shard0 = unitStory(run, 'tests-shard-0', 'passed', 5);
    const shard1 = unitStory(run, 'tests-shard-1', 'failed', 30);
    const all = [...build, ...shard0, ...shard1];

    // Out of order: shard 1's story arrives back to front across batches, shard 0 has a hole until the end.
    const batches = [
        [...shard1.slice(20).reverse(), ...build.slice(0, 2), shard0[0]!],
        [...shard1.slice(10, 20).reverse(), ...shard0.slice(2), ...build.slice(2)],
        [...shard1.slice(0, 10).reverse()],
        [shard0[1]!, build[1]!], // fills shard 0's hole; build 1 is a replay
    ];
    let replays = 0;
    let holding = -1;
    for (const batch of batches) {
        const response = await request(`/runs/${run}/events`, runner, { method: 'POST', body: jsonLines(batch) });
        const result = (await response.json()) as { replays: number; holding: number };
        check(response.status === 200, `events batch of ${batch.length} accepted (${response.status}, holding ${result.holding})`);
        replays += result.replays;
        holding = result.holding;
    }
    check(replays === 1, `the replay was dropped (${replays} replay)`);
    check(holding === 0, `nothing left waiting on a gap (${holding})`);
    const stray = await request(`/runs/${run}/events`, runner, { method: 'POST', body: jsonLines([event('someone-else', 'build', 99, 'started')]) });
    check(stray.status === 400, `an event of another run refused (${stray.status})`);
    const otherRunToken = await tokenFor('someone-else', 'runner', 600);
    const wrongRun = await request(`/runs/${run}/events`, otherRunToken, { method: 'POST', body: jsonLines([build[0]!]) });
    check(wrongRun.status === 403, `a token for another run refused (${wrongRun.status})`);
    const forged = await mintToken('not-the-secret', { run: run, scope: 'runner', expires: Math.floor(Date.now() / 1000) + 600 });
    const forgedResponse = await request(`/runs/${run}/events`, forged, { method: 'POST', body: jsonLines([build[0]!]) });
    check(forgedResponse.status === 401, `a forged token refused (${forgedResponse.status})`);

    await waitFor(() => live.received.size === all.length, 'the live viewer to see every event');
    const late = await openViewer(run, viewerToken);
    await waitFor(() => hasFrame(late, 'caughtUp'), 'the late viewer to catch up');
    check(storyIsWhole(live, all), `live viewer: all ${all.length} events, each once, in order`);
    check(storyIsWhole(late, all), `late viewer: all ${all.length} events, each once, in order`);
    check(late.frames[0]?.kind === 'plan', 'late viewer got the plan first');

    // Blobs, through the run.
    const blobs = `/runs/${run}/blobs`;
    const blob = new Uint8Array(randomBytes(1024 * 1024 + 7));
    const blobHash = sha256Hex(blob);
    const put = await request(`${blobs}/${blobHash}`, runner, { method: 'PUT', body: blob });
    check(put.status === 201, `blob PUT by the runner stored (${put.status})`);
    const putAgain = await request(`${blobs}/${blobHash}`, coordinator, { method: 'PUT', body: blob });
    check(putAgain.status === 200 && ((await putAgain.json()) as { stored: boolean }).stored === false, `blob PUT again left as is (${putAgain.status})`);
    const get = await request(`${blobs}/${blobHash}`, runner);
    const gotten = new Uint8Array(await get.arrayBuffer());
    check(get.status === 200 && sha256Hex(gotten) === blobHash, `blob GET returned the same ${gotten.length} bytes`);
    const inputGet = await request(inputPath, runner);
    check(inputGet.status === 200 && sha256Hex(new Uint8Array(await inputGet.arrayBuffer())) === inputHash, `the runner reads the planned input (${inputGet.status})`);
    const wrongHash = sha256Hex(new Uint8Array(randomBytes(32)));
    const badPut = await request(`${blobs}/${wrongHash}`, runner, { method: 'PUT', body: blob });
    check(badPut.status === 400, `blob PUT with the wrong hash refused (${badPut.status})`);
    const badGet = await request(`${blobs}/${wrongHash}`, coordinator);
    check(badGet.status === 404, `and nothing was stored under it (${badGet.status})`);
    const unplanned = await request(`${blobs}/${wrongHash}`, runner);
    check(unplanned.status === 403, `a runner can't read a hash outside its plan and uploads (${unplanned.status})`);
    const viewerPut = await request(`${blobs}/${blobHash}`, viewerToken, { method: 'PUT', body: blob });
    check(viewerPut.status === 403, `a viewer can't PUT a blob (${viewerPut.status})`);
    const viewerGet = await request(`${blobs}/${blobHash}?token=${encodeURIComponent(viewerToken)}`, null);
    await viewerGet.body?.cancel();
    check(viewerGet.status === 200, `a viewer reads the run's upload by ?token= (${viewerGet.status})`);
    const viewerInput = await request(inputPath, viewerToken);
    await viewerInput.body?.cancel();
    check(viewerInput.status === 200, `a viewer reads the input the coordinator uploaded through this run (${viewerInput.status})`);
    const viewerUnknown = await request(`${blobs}/${wrongHash}`, viewerToken);
    check(viewerUnknown.status === 403, `a viewer can't read a hash the run never uploaded (${viewerUnknown.status})`);
    const otherRun = await request(`${blobs}/${blobHash}`, await tokenFor('someone-else', 'runner', 600));
    check(otherRun.status === 403, `another run's token can't reach this run's blobs (${otherRun.status})`);
    const oldRoute = await request(`/blobs/${blobHash}`, runner);
    check(oldRoute.status === 404, `the v0.1 /blobs route is gone (${oldRoute.status})`);

    // The cache: an entry naming this run's blob as build's output and build's event log, uploaded as a blob.
    const eventLog = new TextEncoder().encode(jsonLines(build));
    const eventLogHash = sha256Hex(eventLog);
    const eventLogPut = await request(`${blobs}/${eventLogHash}`, coordinator, { method: 'PUT', body: eventLog });
    check(eventLogPut.status === 201, `build's event log uploaded as a blob (${eventLogPut.status})`);
    const cacheKey = sha256Hex(new Uint8Array(randomBytes(32)));
    const entry = {
        key: cacheKey,
        run: run,
        unit: 'build',
        machine: 'smoke',
        runnerVersion: 'smoke',
        wallSeconds: 2.25,
        outputs: [{ path: 'smoke.bin', sha256: blobHash, bytes: blob.length }],
        events: eventLogHash,
    };
    const cacheMissing = await request(`/cache/${cacheKey}`, coordinator, { method: 'PUT', body: JSON.stringify({ ...entry, events: wrongHash }) });
    check(cacheMissing.status === 409, `a cache entry naming a blob the store lacks refused (${cacheMissing.status})`);
    const cacheMiss = await request(`/cache/${cacheKey}`, coordinator);
    check(cacheMiss.status === 404, `a cache miss (${cacheMiss.status})`);
    const cachePut = await request(`/cache/${cacheKey}`, coordinator, { method: 'PUT', body: JSON.stringify(entry) });
    check(cachePut.status === 201, `cache entry written (${cachePut.status})`);
    const cacheAgain = await request(`/cache/${cacheKey}`, coordinator, { method: 'PUT', body: JSON.stringify({ ...entry, machine: 'other' }) });
    check(cacheAgain.status === 200, `a second entry under the key left the first as is (${cacheAgain.status})`);
    const cacheHit = await request(`/cache/${cacheKey}`, coordinator);
    const hit = (await cacheHit.json()) as { machine: string };
    check(cacheHit.status === 200 && hit.machine === 'smoke', `cache hit reads the first entry back (${cacheHit.status})`);
    const cacheByRunner = await request(`/cache/${cacheKey}`, runner);
    check(cacheByRunner.status === 403, `a runner can't read the cache (${cacheByRunner.status})`);

    // The verdict, then the archive.
    const verdict = { status: 'red', failed: ['tests-shard-1'], problems: [], cached: [] };
    const verdictResponse = await request(`/runs/${run}/verdict`, coordinator, { method: 'POST', body: JSON.stringify(verdict) });
    check(verdictResponse.status === 201, `verdict accepted (${verdictResponse.status})`);
    await waitFor(() => hasFrame(live, 'verdict') && hasFrame(late, 'verdict'), 'both viewers to get the verdict');
    check(live.frames.at(-1)?.kind === 'verdict' && late.frames.at(-1)?.kind === 'verdict', 'the verdict came last to both viewers');
    const after = await request(`/runs/${run}/events`, runner, { method: 'POST', body: jsonLines([event(run, 'build', 99, 'started')]) });
    check(after.status === 409, `events after the verdict refused (${after.status})`);
    const latePut = await request(`${blobs}/${wrongHash}`, runner, { method: 'PUT', body: blob });
    check(latePut.status === 409, `a runner's blob PUT after the verdict refused (${latePut.status})`);
    live.socket.close();
    late.socket.close();

    const readArchive = (name: string) =>
        execFileSync(
            'npx',
            ['wrangler', 'r2', 'object', 'get', `loom-store/runs/${run}/${name}`, '--remote', '--pipe'],
            { cwd: wireDirectory, env: { ...process.env, CLOUDFLARE_ACCOUNT_ID: accountId }, encoding: 'utf8', stdio: ['ignore', 'pipe', 'ignore'] },
        )
            .trim()
            .split('\n')
            .map((line) => JSON.parse(line) as Record<string, unknown>);
    const archived = readArchive('events.jsonl');
    check(archived.length === all.length, `runs/${run}/events.jsonl is in R2 with ${archived.length} events`);
    const uploads = readArchive('blobs.jsonl');
    const uploadsAre = (sha256: string, scope: string) => uploads.some((line) => line.sha256 === sha256 && line.scope === scope);
    check(
        uploads.length === 3 && uploadsAre(inputHash, 'coordinator') && uploadsAre(blobHash, 'runner') && uploadsAre(eventLogHash, 'coordinator'),
        `runs/${run}/blobs.jsonl lists the ${uploads.length} uploads`,
    );

    console.log(`\nlive page (viewer token, 7 days):\n${baseUrl}/runs/${run}?token=${viewerToken}\n`);
    return run;
}

async function latencyRun(): Promise<void> {
    const run = `latency-${new Date().toISOString().replace(/[-:]/g, '').replace(/\..*$/, '')}-${randomBytes(3).toString('hex')}`;
    console.log(`run ${run}`);
    const coordinator = await tokenFor(run, 'coordinator', daySeconds);
    const runner = await tokenFor(run, 'runner', daySeconds);
    const viewerToken = await tokenFor(run, 'viewer', 7 * daySeconds);
    const units = Array.from({ length: 50 }, (_, index) => `unit-${String(index).padStart(2, '0')}`);
    await request(`/runs/${run}/plan`, coordinator, { method: 'POST', body: JSON.stringify({ units: units, inputs: [] }) });
    const viewer = await openViewer(run, viewerToken);
    await waitFor(() => hasFrame(viewer, 'caughtUp'), 'the latency viewer to catch up');
    const sent = new Map<string, number>();

    async function post(item: Record<string, unknown>): Promise<void> {
        sent.set(`${item.unit} ${item.sequence}`, performance.now());
        const response = await request(`/runs/${run}/events`, runner, { method: 'POST', body: JSON.stringify(item) + '\n' });
        if (response.status !== 200) {
            throw new Error(`latency POST refused: ${response.status} ${await response.text()}`);
        }
    }
    function latencies(keys: string[]): number[] {
        return keys.map((key) => (viewer.received.get(key) ?? NaN) - (sent.get(key) ?? NaN));
    }

    // One at a time: 200 events, round robin over the units, sequences 0 to 3.
    const sequentialKeys: string[] = [];
    for (let index = 0; index < 200; index++) {
        const unit = units[index % 50]!;
        const sequence = Math.floor(index / 50);
        const item = sequence === 0 ? event(run, unit, 0, 'started', { machine: 'smoke' }) : event(run, unit, sequence, 'output', { stream: 'stdout', text: `line ${sequence}` });
        await post(item);
        sequentialKeys.push(`${unit} ${sequence}`);
    }
    await waitFor(() => sequentialKeys.every((key) => viewer.received.has(key)), 'the sequential events to arrive');

    // 50 units at once, each posting its next 4 events one after another: sequences 4 to 7.
    const concurrentKeys: string[] = [];
    await Promise.all(
        units.map(async function (unit) {
            for (let sequence = 4; sequence < 8; sequence++) {
                const item = sequence === 7 ? event(run, unit, 7, 'finished', { status: 'passed' }) : event(run, unit, sequence, 'output', { stream: 'stdout', text: `line ${sequence}` });
                concurrentKeys.push(`${unit} ${sequence}`);
                await post(item);
            }
        }),
    );
    await waitFor(() => concurrentKeys.every((key) => viewer.received.has(key)), 'the concurrent events to arrive');
    check(viewer.received.size === 400, `latency viewer saw all 400 events (${viewer.received.size})`);

    const sequential = summary(latencies(sequentialKeys));
    const concurrent = summary(latencies(concurrentKeys));
    console.log(`latency, 200 events one at a time (ms): ${JSON.stringify(sequential)}`);
    console.log(`latency, 50 units writing at once, 200 events (ms): ${JSON.stringify(concurrent)}`);
    await request(`/runs/${run}/verdict`, coordinator, { method: 'POST', body: JSON.stringify({ status: 'green', failed: [], problems: [], cached: [] }) });
    viewer.socket.close();
    console.log(`latency page: ${baseUrl}/runs/${run}?token=${viewerToken}`);
}

console.log(`wire ${baseUrl}`);
await smokeRun();
await latencyRun();
console.log(failures === 0 ? '\nevery check passed' : `\n${failures} checks failed`);
process.exit(failures === 0 ? 0 : 1);
