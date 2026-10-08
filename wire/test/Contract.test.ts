import { describe, expect, it } from 'vitest';
import goEvents from '../../protocol/testdata/events.jsonl?raw';
import { call, postPlan, token } from './Helpers';

// protocol/testdata/events.jsonl is what Go writes (TestEventsFixtureIsWhatGoWrites): zeros left off, the wire and
// run error phases, a timed-out exit. The Worker must take every line of it as it stands.
describe('the Go contract', function () {
    it('accepts every event Go writes, and the verdict Go marshals', async function () {
        const run = 'r-fixture';
        const lines = goEvents.trim().split('\n');
        const response = await call(`/runs/${run}/events`, { method: 'POST', bearer: await token(run, 'runner'), body: goEvents });
        expect(response.status, await response.clone().text()).toBe(200);
        expect(await response.json()).toMatchObject({ accepted: lines.length, released: lines.length, holding: 0 });
        const coordinator = await token(run, 'coordinator');
        await postPlan(run, coordinator, ['a', 'tests[shard=0]']);
        // protocol.Verdict's MarshalJSON, as TestVerdictIsLowercaseWithEmptyListsAsArrays pins it.
        const verdict = '{"status":"red","failed":["tests[shard=0]"],"problems":[]}';
        expect((await call(`/runs/${run}/verdict`, { method: 'POST', bearer: coordinator, body: verdict })).status).toBe(201);
    });
});
