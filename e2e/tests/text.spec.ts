import { test, expect } from '../fixtures';

// What the row-count assertion pins.
const textFixtureLines = 30;

// Spelled out rather than generated, so a fixture loop that misnumbers its
// lines cannot misnumber the expectation with it.
const wantLine17 = 'line 17 of 30 in the plain text fixture';

function textFixture() {
  let s = '';
  for (let i = 1; i <= textFixtureLines; i++) s += `line ${i} of 30 in the plain text fixture\n`;
  return s;
}

// The text shell renders a numbered, line-addressable document: every line an
// anchor, a gutter click that cites a line in the fragment, and a #L10-L20
// arrival that selects the range and brings it into view.
test('text render', async ({ server, page, pageErrors, shot }) => {
  const paste = server.upload(textFixture(), { type: 'txt', name: 'text proof' });
  await page.goto(paste.url);

  // aria-busy flips false only after the rows are appended, and the error path
  // writes no .row at all, so the pair matches nothing but a finished render.
  await expect(page.locator('#text[aria-busy="false"] .row').first()).toBeVisible();
  // Each row's anchor id and data-line is checked against its position, so a
  // gutter that numbered rows wrong fails as a count rather than passing on
  // mere presence.
  const got = await page.evaluate(() => {
    const rows = Array.from(document.querySelectorAll<HTMLElement>('#text .row'));
    const bad = rows.filter((r, i) => r.id !== 'L' + (i + 1) || r.dataset.line !== String(i + 1));
    const l17 = document.querySelector('#L17 .lg-code');
    return {
      rows: rows.length,
      badAnchors: bad.length,
      preSelected: document.querySelectorAll('#text .row.lg-sel').length,
      line17: l17 ? l17.textContent : '',
      meta: document.getElementById('meta')!.textContent,
    };
  });
  await shot('plain');

  expect.soft(got.rows, 'line rows').toBe(textFixtureLines);
  expect.soft(got.badAnchors, 'rows whose anchor id or data-line disagrees with their position').toBe(0);
  expect.soft(got.preSelected, 'rows selected before any interaction').toBe(0);
  expect.soft(got.line17).toBe(wantLine17);
  expect.soft(got.meta).toBe('30 lines');

  // A live gutter, not a painted one: clicking line 5's number must mark the
  // row and write the citation into the fragment.
  await page.locator('#L5 .lg-num').click();
  await page.waitForFunction(
    () =>
      location.hash === '#L5' &&
      Array.from(document.querySelectorAll<HTMLElement>('#text .row.lg-sel'), (r) => r.dataset.line).join() === '5',
  );
  await shot('line-selected');

  // Through about:blank so the fragment arrives on a cross-document load, the
  // cited-link case deeplink.js exists for: the browser's own fragment attempt
  // runs against an empty document, and the shell must resolve it after the
  // rows exist. A fragment-only navigation would instead exercise the
  // hashchange path on an already-rendered page.
  await page.goto('about:blank');
  await page.goto(paste.url + '#L10-L20');

  await expect(page.locator('#text .row.lg-sel').first()).toBeVisible();
  // Polled, not snapshotted: scrollIntoView animates, so the scroll offset is
  // not readable right after the selection appears. The in-view arm is what a
  // page too short to scroll settles on.
  await page.waitForFunction(
    () =>
      window.scrollY > 0 ||
      (() => {
        const a = document.getElementById('L10');
        if (!a) return false;
        const r = a.getBoundingClientRect();
        return r.top >= 0 && r.bottom <= window.innerHeight;
      })(),
  );
  const dl = await page.evaluate(() => {
    const a = document.getElementById('L10');
    const r = a ? a.getBoundingClientRect() : { top: -1, bottom: -1 };
    return {
      selected: Array.from(document.querySelectorAll<HTMLElement>('#text .row.lg-sel'), (row) => row.dataset.line),
      arrived: document.querySelectorAll('#text .row.lg-arrive').length,
      hash: location.hash,
      scrollY: window.scrollY,
      anchorTop: r.top,
      anchorBot: r.bottom,
      viewportH: window.innerHeight,
    };
  });
  await shot('deep-linked');

  const wantSel = ['10', '11', '12', '13', '14', '15', '16', '17', '18', '19', '20'];
  expect.soft(dl.selected).toEqual(wantSel);
  expect.soft(dl.arrived, 'rows carrying the arrival flash').toBe(wantSel.length);
  expect.soft(dl.hash).toBe('#L10-L20');
  expect
    .soft(
      dl.scrollY > 0 || (dl.anchorTop >= 0 && dl.anchorBot <= dl.viewportH),
      `anchor L10 neither scrolled to nor in view: scrollY=${dl.scrollY} top=${dl.anchorTop} bottom=${dl.anchorBot} viewport=${dl.viewportH}`,
    )
    .toBe(true);

  pageErrors.expectNone();
});
