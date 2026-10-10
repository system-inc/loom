// Every inline script on every page the Workers serve compiles: a syntax error in a page's script would otherwise pass
// every Worker test and blank the page in the browser. node:vm compiles each script without running it.

import { Script } from 'node:vm';
import { describe, expect, it } from 'vitest';
import { renderBoardPage } from '../../source/BoardPage';
import { renderLivePage } from '../../source/LivePage';
import { renderLoomLivePage } from '../../source/LoomLivePage';

const nonce = 'testnonce';

function inlineScripts(html: string): string[] {
    return [...html.matchAll(/<script\b[^>]*>([\s\S]*?)<\/script>/g)].map(function (match) {
        return match[1] ?? '';
    });
}

describe("the pages' inline scripts", function () {
    for (const [name, render] of [
        ['Loom Live, at / on loom', renderLoomLivePage],
        ['the board, at /board on loom-runs', renderBoardPage],
        ['a run, at /runs/<run> on loom-runs', renderLivePage],
    ] as const) {
        it(`compile on ${name}`, function () {
            const scripts = inlineScripts(render(nonce));
            expect(scripts.length).toBeGreaterThan(0);
            for (const script of scripts) {
                expect(function () {
                    return new Script(script, { filename: name });
                }).not.toThrow();
            }
        });
    }

    it('would catch a syntax error', function () {
        expect(function () {
            return new Script(inlineScripts(renderLoomLivePage(nonce).replace("'use strict';", "'use strict'; var ;"))[0] ?? '');
        }).toThrow(SyntaxError);
    });
});
