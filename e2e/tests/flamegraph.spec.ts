import { test, expect } from '../fixtures';
import type { Page } from '@playwright/test';

// Two folded stacks under one parent, with a 3:1 sample split so the child
// widths have an order a broken layout could get wrong. The tree the shell
// should build: all(40) -> profileMain(40) -> encodeFrames(30) + flushBuffers(10).
const flamegraphFixture = `profileMain;encodeFrames 30
profileMain;flushBuffers 10
`;

// Cannot match the unrendered page: shell.html hard-codes aria-busy="true" on
// the mount, and only flame.js flips it, on a render, an empty profile, or a
// fetch error alike. Waiting on it reports a failed load as the text the shell
// wrote instead of as a timeout.
const flameSettled = '#flame[aria-busy="false"]';

// One rendered frame box. Width is the box's fraction of the mount, so
// assertions compare shares rather than pixels.
type FlameFrame = { name: string; chain: boolean; width: number; top: number };

// The semantic readout of the whole shell: the frame boxes, the breadcrumb
// trail, and the focused share, read in one pass.
type FlameState = { mountText: string; frames: FlameFrame[]; crumbs: string[]; share: string };

function probeFlame(page: Page): Promise<FlameState> {
  return page.evaluate(() => {
    const mount = document.getElementById('flame')!;
    const mw = mount.getBoundingClientRect().width;
    const frames = [...mount.querySelectorAll<HTMLElement>('.fr')].map((el) => ({
      name: el.dataset.name!,
      chain: el.classList.contains('chain'),
      width: el.getBoundingClientRect().width / mw,
      top: parseFloat(el.style.top),
    }));
    return {
      mountText: frames.length ? '' : mount.textContent!.trim(),
      frames,
      crumbs: [...document.querySelectorAll('#crumbs .crumb')].map((e) => e.textContent!.trim()),
      share: (document.querySelector('#crumbs .share')?.textContent ?? '').trim(),
    };
  });
}

// Names come widest-first per row, which is the layout order the renderer
// commits to, so an ordered equality pins it.
const frameNames = (frames: FlameFrame[]) => frames.map((f) => f.name);
const frameIndex = (frames: FlameFrame[]) => Object.fromEntries(frames.map((f) => [f.name, f]));

// Absorbs the sub-pixel rounding of a percentage width without letting a wrong
// share through: the closest wrong value differs by 1/40.
const near = (got: number, want: number) => Math.abs(got - want) < 0.01;

// The flamegraph shell lays frames out as width-is-share boxes, zooms into a
// clicked frame, and resets back to the whole profile.
test('flamegraph render', async ({ server, page, pageErrors, shot }) => {
  const paste = server.upload(flamegraphFixture, { type: 'flamegraph', name: 'flamegraph proof' });
  await page.goto(paste.url);

  await expect(page.locator(flameSettled)).toBeAttached();
  const got = await probeFlame(page);
  await shot('initial');

  expect(got.frames.length, `shell settled with no frames: ${JSON.stringify(got.mountText)}\n` +
    `page errors: ${pageErrors.list().join('\n')}`).toBeGreaterThan(0);
  expect(frameNames(got.frames)).toEqual(['all', 'profileMain', 'encodeFrames', 'flushBuffers']);
  const byName = frameIndex(got.frames);
  const { all, profileMain: main, encodeFrames: encode, flushBuffers: flush } = byName;
  expect.soft(all.chain, 'synthetic root is marked as chain context').toBe(true);
  expect.soft(near(all.width, 1) && near(main.width, 1),
    `root row widths = ${all.width.toFixed(3)} and ${main.width.toFixed(3)}, want both 1: ` +
    'the root does not span the profile').toBe(true);
  // Width is share: 30 vs 10 samples of a 40-sample profile.
  expect.soft(encode.width, 'encodeFrames wider than flushBuffers with 3x the samples')
    .toBeGreaterThan(flush.width);
  expect.soft(near(encode.width, 0.75) && near(flush.width, 0.25),
    `child widths = ${encode.width.toFixed(3)} and ${flush.width.toFixed(3)}, want 0.75 and 0.25`).toBe(true);
  // Depth is the y axis: children share a row below their parent.
  expect.soft(all.top < main.top && main.top < encode.top && encode.top === flush.top,
    `row tops = all ${all.top}, profileMain ${main.top}, children ${encode.top} and ${flush.top}: ` +
    'rows out of order').toBe(true);
  expect.soft({ crumbs: got.crumbs, share: got.share }, 'crumbs and the full profile share')
    .toEqual({ crumbs: ['all'], share: '100.0% of 40 samples' });

  // Zoom: click the narrow child. The wait cannot match the pre-zoom page,
  // where flushBuffers exists but is not chain context, so a click that never
  // reached the handler fails here rather than as a stale probe.
  await page.locator('#flame .fr[data-name="flushBuffers"]').click();
  await expect(page.locator('#flame .fr.chain[data-name="flushBuffers"]')).toBeAttached();
  const zoomed = await probeFlame(page);
  await shot('zoomed');

  expect(frameNames(zoomed.frames), 'zoomed frames are the focus chain only')
    .toEqual(['all', 'profileMain', 'flushBuffers']);
  for (const f of zoomed.frames) {
    expect.soft(f.chain && near(f.width, 1),
      `zoomed frame ${f.name}: chain=${f.chain} width=${f.width.toFixed(3)}, want full-width chain context`)
      .toBe(true);
  }
  expect.soft(zoomed.crumbs, 'zoomed crumbs are the focused stack').toEqual(['all', 'profileMain', 'flushBuffers']);
  expect.soft(zoomed.share).toBe('25.0% of 40 samples');

  // Reset restores the whole profile. encodeFrames is absent while zoomed, so
  // its reappearance is the reset having rendered.
  await page.locator('#reset').click();
  await expect(page.locator('#flame .fr[data-name="encodeFrames"]')).toBeAttached();
  const reset = await probeFlame(page);
  await shot('reset');

  expect(frameNames(reset.frames), 'frames after reset are the whole profile')
    .toEqual(['all', 'profileMain', 'encodeFrames', 'flushBuffers']);
  const f = frameIndex(reset.frames).flushBuffers;
  expect.soft(!f.chain && near(f.width, 0.25),
    `flushBuffers after reset: chain=${f.chain} width=${f.width.toFixed(3)}, want a plain quarter-width frame`)
    .toBe(true);

  pageErrors.expectNone();
});
