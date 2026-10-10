// Loom's mark, "Into the loom" (Kirk's pick, Oct 10 2026): threads from five far lights narrow onto the loom's beam,
// turn straight and become cloth. Drawn on a 64-unit grid; the accent is the threads, the ink the beam, warp and cloth.

export const LoomGold = '#f2c14e';
export const LoomCream = '#fff4d6';
export const LoomIndigo = '#1a1540';

export function loomMark(size: number, ink = LoomCream, accent = LoomGold): string {
    return `<svg width="${size}" height="${size}" viewBox="0 0 64 64" fill="none" stroke-linecap="round" role="img" aria-label="Loom">`
        + `<g stroke="${accent}" stroke-width="2"><line x1="6" y1="6" x2="22" y2="32"/><line x1="19" y1="6" x2="27" y2="32"/>`
        + `<line x1="32" y1="6" x2="32" y2="32"/><line x1="45" y1="6" x2="37" y2="32"/><line x1="58" y1="6" x2="42" y2="32"/></g>`
        + `<g fill="${accent}"><circle cx="6" cy="6" r="2.4"/><circle cx="19" cy="6" r="2.4"/><circle cx="32" cy="6" r="2.4"/>`
        + `<circle cx="45" cy="6" r="2.4"/><circle cx="58" cy="6" r="2.4"/></g>`
        + `<line x1="17" y1="32" x2="47" y2="32" stroke="${ink}" stroke-width="4.5"/>`
        + `<g stroke="${ink}" stroke-width="2.4"><line x1="22" y1="32" x2="22" y2="58"/><line x1="27" y1="32" x2="27" y2="58"/>`
        + `<line x1="32" y1="32" x2="32" y2="58"/><line x1="37" y1="32" x2="37" y2="58"/><line x1="42" y1="32" x2="42" y2="58"/></g>`
        + `<rect x="19" y="44" width="26" height="14" rx="2" fill="${ink}"/></svg>`;
}

// The favicon: the mark on its indigo tile, so it reads on a light tab and a dark one.
export function loomFavicon(): string {
    return `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 64 64"><rect width="64" height="64" rx="14" fill="${LoomIndigo}"/>`
        + `<g transform="translate(6 6) scale(0.8125)">${loomMark(64).replace(/^<svg[^>]*>/, '').replace(/<\/svg>$/, '')}</g></svg>`;
}
