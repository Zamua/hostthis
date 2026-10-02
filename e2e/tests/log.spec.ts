import { test, expect } from '../fixtures';

// 20 NDJSON records with errors at known positions (4, 9, 15, 19), so the
// filter assertion can name the exact rows it must keep rather than only a
// count. Timestamps step evenly by 30s, which lands every record in its own
// histogram bucket and makes the nonzero-bucket count exact.
const logFixture = `{"ts":"2026-08-17T10:00:00Z","level":"info","msg":"server listening on :8080","service":"api"}
{"ts":"2026-08-17T10:00:30Z","level":"warn","msg":"config key retries missing, using default","service":"api"}
{"ts":"2026-08-17T10:01:00Z","level":"info","msg":"GET /healthz 200","service":"api"}
{"ts":"2026-08-17T10:01:30Z","level":"error","msg":"connection reset by peer","service":"api"}
{"ts":"2026-08-17T10:02:00Z","level":"info","msg":"GET /p/abc123 200","service":"api"}
{"ts":"2026-08-17T10:02:30Z","level":"info","msg":"cache miss for slug abc123","service":"worker"}
{"ts":"2026-08-17T10:03:00Z","level":"warn","msg":"slow query took 1200ms","service":"worker"}
{"ts":"2026-08-17T10:03:30Z","level":"info","msg":"sweep pass started","service":"worker"}
{"ts":"2026-08-17T10:04:00Z","level":"error","msg":"upstream timeout after 5s","service":"api"}
{"ts":"2026-08-17T10:04:30Z","level":"info","msg":"sweep pass finished","service":"worker"}
{"ts":"2026-08-17T10:05:00Z","level":"info","msg":"POST /upload 201","service":"api"}
{"ts":"2026-08-17T10:05:30Z","level":"warn","msg":"disk usage at 81 percent","service":"worker"}
{"ts":"2026-08-17T10:06:00Z","level":"info","msg":"GET /p/def456 200","service":"api"}
{"ts":"2026-08-17T10:06:30Z","level":"info","msg":"metadata compaction started","service":"worker"}
{"ts":"2026-08-17T10:07:00Z","level":"error","msg":"write failed: disk full","service":"worker"}
{"ts":"2026-08-17T10:07:30Z","level":"info","msg":"metadata compaction finished","service":"worker"}
{"ts":"2026-08-17T10:08:00Z","level":"warn","msg":"retrying flush, attempt 2","service":"worker"}
{"ts":"2026-08-17T10:08:30Z","level":"info","msg":"GET /p/ghi789 200","service":"api"}
{"ts":"2026-08-17T10:09:00Z","level":"error","msg":"tls handshake failed","service":"api"}
{"ts":"2026-08-17T10:09:30Z","level":"info","msg":"shutdown signal ignored, draining","service":"api"}
`;

// Matches the four error records and nothing else.
const logQuery = 'level=error';

// The shell's own end-of-work mark. The static page ships aria-busy="true" and
// every terminal path (rendered, empty file, failed fetch) flips it, so a
// broken load reports the shell's message instead of a timeout.
const logSettled = '#log[aria-busy="false"]';

// The log shell parses NDJSON into one row per record with level chips and a
// time histogram, and the query box narrows the view to exactly the matching
// records and restores it when cleared.
test('log render', async ({ server, page, pageErrors, shot }) => {
  const paste = server.upload(logFixture, { type: 'log', name: 'log proof' });
  await page.goto(paste.url);

  await expect(page.locator(logSettled)).toBeVisible();
  await shot('rendered');

  // nonzero and lit count the histogram bars whose height the viewer set above
  // 0%, separately for the grey all-records layer and the coloured matching layer.
  const got = await page.evaluate(() => {
    const hist = document.getElementById('hist')!;
    const nz = (sel: string) =>
      Array.from(hist.querySelectorAll<HTMLElement>(sel)).filter((e) => parseFloat(e.style.height) > 0).length;
    return {
      recs: document.querySelectorAll('#log .rec').length,
      meta: document.getElementById('meta')!.textContent,
      chips: Array.from(document.querySelectorAll('#levels .chip'), (e) => e.textContent),
      histShown: !hist.hidden,
      buckets: hist.querySelectorAll('.hb').length,
      nonzero: nz('.hb-all'),
      lit: nz('.hb-lit'),
    };
  });
  expect(got.recs, `#log: ${JSON.stringify(await page.locator('#log').textContent())}`).toBe(20);
  expect.soft(got.meta).toBe('20 records');
  // Chip order is the viewer's severity order, and each count is a parse
  // result: a chip totals the records it classified into that level.
  expect.soft(got.chips).toEqual(['ERROR 4', 'WARN 4', 'INFO 12']);
  expect.soft(got.histShown, 'histogram is hidden: no timestamps were parsed').toBe(true);
  expect.soft(got.buckets, 'histogram buckets').toBe(60);
  // One bucket per record by the fixture's even spacing, in both layers, since
  // with no query every record matches.
  expect.soft([got.nonzero, got.lit], 'nonzero buckets all / lit').toEqual([20, 20]);

  // Waits ride the meta line as well as the row count: meta is written in the
  // same render pass, so matching both means the pass for the FINAL query
  // finished, not a transient one for a keystroke prefix.
  await page.locator('#q').pressSequentially(logQuery);
  await page.waitForFunction(
    () =>
      document.querySelectorAll('#log .rec').length === 4 &&
      document.getElementById('meta')!.textContent === '4 of 20',
  );
  await shot('filtered');

  // Row ids carry the SOURCE record number, so they name which records
  // survived the filter, not just how many.
  const f = await page.evaluate(() => ({
    ids: Array.from(document.querySelectorAll('#log .rec'), (e) => e.id),
    errorRows: document.querySelectorAll('#log .rec.lv-error').length,
    meta: document.getElementById('meta')!.textContent,
    lit: Array.from(document.querySelectorAll<HTMLElement>('#hist .hb-lit')).filter(
      (e) => parseFloat(e.style.height) > 0,
    ).length,
  }));
  expect.soft(f.ids).toEqual(['L4', 'L9', 'L15', 'L19']);
  expect.soft(f.errorRows, 'filtered rows carrying lv-error').toBe(f.ids.length);
  expect.soft(f.meta).toBe('4 of 20');
  // The coloured layer follows the query while the grey layer keeps every
  // record, so a narrowed view lights exactly the matching buckets.
  expect.soft(f.lit, 'filtered histogram lit buckets').toBe(4);

  // Real backspaces rather than a scripted value reset: the shell re-renders on
  // the input events a user produces.
  for (let i = 0; i < logQuery.length; i++) await page.locator('#q').press('Backspace');
  await page.waitForFunction(
    () =>
      document.querySelectorAll('#log .rec').length === 20 &&
      document.getElementById('meta')!.textContent === '20 records',
  );
  await shot('restored');

  pageErrors.expectNone();
});
