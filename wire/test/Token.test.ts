import { describe, expect, it } from 'vitest';
import { decodeBase64Url, encodeBase64Url, mintToken, verifyToken } from '../source/Token';
import { call, freshRun, postEvents, TestSecret, token, unitEvents } from './Helpers';

// The vector TestTokenVector pins in protocol/protocol_test.go.
const vector =
    'eyJydW4iOiJyLXZlY3RvciIsInNjb3BlIjoicnVubmVyIiwiZXhwaXJlcyI6NDEwMjQ0NDgwMH0.5yFFC9AOwC9L6zqwWz8V0aCJNrLwusF5LjbOv_hoDWY';
const vectorNow = 1791486000;

async function signedClaims(payload: string): Promise<string> {
    // Signs arbitrary claim bytes, to show the claims check stands on its own behind a good signature.
    const firstPart = encodeBase64Url(new TextEncoder().encode(payload));
    const key = await crypto.subtle.importKey(
        'raw',
        new TextEncoder().encode(TestSecret),
        { name: 'HMAC', hash: 'SHA-256' },
        false,
        ['sign'],
    );
    const signature = await crypto.subtle.sign('HMAC', key, new TextEncoder().encode(firstPart));
    return firstPart + '.' + encodeBase64Url(new Uint8Array(signature));
}

describe('tokens', function () {
    it('mints the pinned vector byte for byte', async function () {
        expect(await mintToken(TestSecret, { run: 'r-vector', scope: 'runner', expires: 4102444800 })).toBe(vector);
    });

    it('verifies the pinned vector', async function () {
        const verification = await verifyToken(TestSecret, vector, vectorNow);
        expect(verification).toEqual({ valid: true, claims: { run: 'r-vector', scope: 'runner', expires: 4102444800 } });
    });

    it('refuses tampering, other secrets, bad shapes and expiry', async function () {
        const [claimsPart, signaturePart] = vector.split('.') as [string, string];
        const refused: Record<string, [string, string, number]> = {
            'tampered claims': [TestSecret, vector.replace('eyJydW4iOiJy', 'eyJydW4iOiJz'), vectorNow],
            'other secret': ['other', vector, vectorNow],
            'no signature': [TestSecret, claimsPart, vectorNow],
            'short signature': [TestSecret, claimsPart + '.AAAA', vectorNow],
            'flipped signature byte': [
                TestSecret,
                claimsPart + '.' + (signaturePart[0] === 'A' ? 'B' : 'A') + signaturePart.slice(1),
                vectorNow,
            ],
            'padded signature': [TestSecret, vector + '=', vectorNow],
            'non-canonical signature tail': [TestSecret, claimsPart + '.' + signaturePart.slice(0, -1) + 'X', vectorNow],
            'expired at the second': [TestSecret, vector, 4102444800],
            'expired after': [TestSecret, vector, 4102444801],
            'empty': [TestSecret, '', vectorNow],
        };
        for (const [name, [secret, candidate, now]] of Object.entries(refused)) {
            const verification = await verifyToken(secret, candidate, now);
            expect(verification.valid, name).toBe(false);
        }
    });

    it('refuses well-signed claims that are not exactly run, scope and expires', async function () {
        const shapes = [
            '{"run":"r","scope":"admin","expires":4102444800}',
            '{"run":"","scope":"runner","expires":4102444800}',
            '{"run":"r","scope":"runner"}',
            '{"run":"r","scope":"runner","expires":"4102444800"}',
            '{"run":"r","scope":"runner","expires":4102444800.5}',
            '{"run":"r","scope":"runner","expires":4102444800,"admin":true}',
            '["r","runner",4102444800]',
            '{"run":"r","scope":"runner","expires":4102444800} x',
        ];
        for (const shape of shapes) {
            const verification = await verifyToken(TestSecret, await signedClaims(shape), vectorNow);
            expect(verification.valid, shape).toBe(false);
        }
        const good = await verifyToken(TestSecret, await signedClaims('{"run":"r","scope":"viewer","expires":4102444800}'), vectorNow);
        expect(good.valid).toBe(true);
    });

    it('decodes base64url strictly', function () {
        expect(decodeBase64Url('AA')).toEqual(new Uint8Array([0]));
        expect(decodeBase64Url('AB')).toBeNull(); // trailing bits set
        expect(decodeBase64Url('AA==')).toBeNull();
        expect(decodeBase64Url('A+')).toBeNull();
        expect(decodeBase64Url('A')).toBeNull();
    });
});

describe('the Worker checks every token', function () {
    it('refuses a token for a different run than the URL', async function () {
        const run = freshRun();
        const other = freshRun();
        const response = await postEvents(run, await token(other, 'runner'), unitEvents(run, 'a'));
        expect(response.status).toBe(403);
        const page = await call(`/runs/${run}?token=${encodeURIComponent(await token(other, 'viewer'))}`);
        expect(page.status).toBe(403);
    });

    it('refuses a missing, forged or expired token, and the wrong scope', async function () {
        const run = freshRun();
        const events = unitEvents(run, 'a');
        expect((await postEvents(run, '', events)).status).toBe(401);
        const forged = await mintToken('not-the-secret', { run: run, scope: 'runner', expires: 4102444800 });
        expect((await postEvents(run, forged, events)).status).toBe(401);
        const expired = await mintToken(TestSecret, { run: run, scope: 'runner', expires: Math.floor(Date.now() / 1000) - 1 });
        expect((await postEvents(run, expired, events)).status).toBe(401);
        expect((await postEvents(run, await token(run, 'viewer'), events)).status).toBe(403);
        const plan = await call(`/runs/${run}/plan`, {
            method: 'POST',
            bearer: await token(run, 'runner'),
            body: '{"units":["a"]}',
        });
        expect(plan.status).toBe(403);
        // A token in the query string only counts for the page and its stream.
        const queried = await call(`/runs/${run}/events?token=${encodeURIComponent(await token(run, 'runner'))}`, {
            method: 'POST',
            body: JSON.stringify(events[0]),
        });
        expect(queried.status).toBe(401);
        expect((await postEvents(run, await token(run, 'runner'), events)).status).toBe(200);
    });

    it('serves the live page to any scope of the run', async function () {
        const run = freshRun();
        for (const scope of ['viewer', 'runner', 'coordinator'] as const) {
            const page = await call(`/runs/${run}?token=${encodeURIComponent(await token(run, scope))}`);
            expect(page.status).toBe(200);
            expect(page.headers.get('Content-Type')).toContain('text/html');
            const html = await page.text();
            expect(html).toContain('prefers-color-scheme: dark');
            expect(html).not.toContain(run); // the run id is read by the script, never written into the HTML
            expect(html).not.toContain('—'); // no em-dashes in the copy
        }
        expect((await call(`/runs/${run}`)).status).toBe(401);
    });
});
