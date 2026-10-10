// The replay proof on a real log (#ey1ay4f). Run from wire/:
//   node --experimental-transform-types scripts/replay.ts <log.jsonl> [--head <hash>] [--main <sha>] [--mutate <seq>]
// (Queue.ts has constructor parameter properties, which plain type stripping refuses.)
//
// Replays a queue log from genesis with Queue.ts's own replay(), not a copy of it, so the proof is of the code the
// Durable Object runs. Every link of the hash chain is checked as it goes, and a broken link is an error. The log is
// the queue's whole log, one event per line, as stored. It prints the replayed seq, head hash, landed changes and the
// last landed main. With --head and --main, it compares them with what the live object reports and exits 1 on any
// difference. Exit 2 when the chain breaks or the file can't be read.
//
// --mutate <seq> alters that event's data before replaying, the mutant: a chain that still replays with an event
// changed would mean the hashes prove nothing. Altering any event but the last breaks the next event's prev; altering
// the last changes the head, so a run with --mutate and --head must never read identical.

import { registerHooks } from 'node:module';
import { readFileSync } from 'node:fs';

// Queue.ts imports the Workers runtime and its siblings without extensions. Outside the runtime, the one class it
// takes from there (DurableObject) is never constructed here, so a stand-in is enough.
registerHooks({
    resolve(specifier, context, nextResolve) {
        if (specifier === 'cloudflare:workers') {
            return { url: 'data:text/javascript,export class DurableObject {}', shortCircuit: true };
        }
        if (specifier.startsWith('.') && !/\.[a-z]+$/.test(specifier)) {
            return nextResolve(specifier + '.ts', context);
        }
        return nextResolve(specifier, context);
    },
});

const queue = await import('../source/Queue.ts');

function argument(name: string): string | undefined {
    const index = process.argv.indexOf(name);
    return index === -1 ? undefined : process.argv[index + 1];
}

const path = process.argv[2];
if (path === undefined || path.startsWith('--')) {
    console.log('usage: node --experimental-transform-types scripts/replay.ts <log.jsonl> [--head <hash>] [--main <sha>] [--mutate <seq>]');
    process.exit(2);
}

let events: Parameters<typeof queue.replay>[0];
try {
    events = readFileSync(path, 'utf8')
        .split('\n')
        .filter(function (line) {
            return line.trim() !== '';
        })
        .map(function (line) {
            return JSON.parse(line);
        });
}
catch (error) {
    console.log(`replay: unreadable: ${String(error)}`);
    process.exit(2);
}

const mutate = argument('--mutate');
if (mutate !== undefined) {
    const target = events.find(function (event) {
        return event.seq === Number(mutate);
    });
    if (target === undefined) {
        console.log(`replay: no event ${mutate} to mutate`);
        process.exit(2);
    }
    target.data = { ...target.data, mutant: true };
}

let state: Awaited<ReturnType<typeof queue.replay>>;
try {
    state = await queue.replay(events);
}
catch (error) {
    console.log(`replay: chain broken: ${error instanceof Error ? error.message : String(error)}`);
    process.exit(2);
}

const landed = [...state.changes.values()].filter(function (entry) {
    return entry.state === 'landed';
});
const lastMain = landed.length === 0 ? null : (landed[landed.length - 1]?.landed ?? null);
console.log(`replay: ${events.length} events to seq ${state.seq}, head ${state.head}, ${state.changes.size} changes, ${landed.length} landed, last main ${lastMain ?? 'none'}`);

const head = argument('--head');
const main = argument('--main');
let differs = false;
if (head !== undefined && head !== state.head) {
    console.log(`replay: head differs: live ${head}, replayed ${state.head}`);
    differs = true;
}
if (main !== undefined && main !== (lastMain ?? '')) {
    console.log(`replay: main differs: live ${main}, replayed ${lastMain ?? 'none'}`);
    differs = true;
}
if (head !== undefined || main !== undefined) {
    console.log(differs ? 'replay: differs from the live object' : 'replay: identical to the live object');
}
process.exit(differs ? 1 : 0);
