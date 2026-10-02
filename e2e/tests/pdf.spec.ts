import { test, expect } from '../fixtures';

// The only string the document draws, distinctive enough that finding it in the
// text layer cannot match the shell's own chrome. It is spliced into a PDF
// literal string, so it must stay free of ( ) and \.
const pdfFixtureText = 'hostthis pdf proof';

// A complete one-page PDF drawing pdfFixtureText in Helvetica. The stream
// /Length and the xref offsets are literal byte counts of THIS layout; an edit
// to any object body must recompute them.
const pdfFixture = Buffer.from(
  '%PDF-1.4\n' +
    '1 0 obj\n<< /Type /Catalog /Pages 2 0 R >>\nendobj\n' +
    '2 0 obj\n<< /Type /Pages /Kids [3 0 R] /Count 1 >>\nendobj\n' +
    '3 0 obj\n<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Contents 4 0 R ' +
    '/Resources << /Font << /F1 5 0 R >> >> >>\nendobj\n' +
    '4 0 obj\n<< /Length 49 >>\nstream\n' +
    'BT /F1 24 Tf 72 720 Td (' + pdfFixtureText + ') Tj ET\n' +
    'endstream\nendobj\n' +
    '5 0 obj\n<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>\nendobj\n' +
    'xref\n0 6\n' +
    '0000000000 65535 f \n' +
    '0000000009 00000 n \n' +
    '0000000058 00000 n \n' +
    '0000000115 00000 n \n' +
    '0000000241 00000 n \n' +
    '0000000340 00000 n \n' +
    'trailer\n<< /Size 6 /Root 1 0 R >>\nstartxref\n410\n%%EOF\n',
  'latin1',
);

// The pdf shell parses the document on a worker, paints page 1 to a canvas
// with a selectable text layer over it, reports the page count, and leaves
// both paging controls disabled on a one-page document.
test('pdf render', async ({ server, page, pageErrors, shot }) => {
  const paste = server.upload(pdfFixture, { type: 'pdf', name: 'pdf proof' });
  await page.goto(paste.url);

  // True once the shell reached an end state. The text layer is written only
  // after pdf.js painted the page's canvas, so the fixture text appearing in it
  // proves the worker parsed the document and page 1 rendered; before the
  // script runs #content holds only the loading .status div, which matches
  // neither branch.
  await expect
    .poll(() =>
      page.evaluate(
        (want) =>
          !!document.querySelector('#content .err') ||
          [...document.querySelectorAll('#content .textLayer span')].some((s) => s.textContent!.includes(want)),
        pdfFixtureText,
      ),
    )
    .toBe(true);
  const got = await page.evaluate(() => {
    const text = (el: Element | null) => (el ? el.textContent!.trim() : '');
    const canvas = document.querySelector<HTMLCanvasElement>('#content .page canvas');
    const box = canvas ? canvas.getBoundingClientRect() : { width: 0, height: 0 };
    return {
      err: text(document.querySelector('#content .err')),
      pages: document.querySelectorAll('#content .page').length,
      pos: text(document.getElementById('pos')),
      canvasW: canvas ? canvas.width : 0,
      canvasH: canvas ? canvas.height : 0,
      boxW: box.width,
      boxH: box.height,
      layerText: [...document.querySelectorAll('#content .textLayer span')].map((s) => s.textContent).join(' '),
      prevDisabled: (document.getElementById('prev') as HTMLButtonElement).disabled,
      nextDisabled: (document.getElementById('next') as HTMLButtonElement).disabled,
    };
  });
  await shot('rendered');

  expect(got.err, 'shell rendered its failure message').toBe('');
  expect.soft(got.pages, 'page holders').toBe(1);
  // Pre-render the counter reads the en-dash placeholder, so this value only
  // exists once the document's page count came back from the worker.
  expect.soft(got.pos, 'page counter').toBe('1 / 1');
  // The paint proof is the text layer above; the dimensions pin that the
  // painted canvas also lays out on screen rather than being collapsed away.
  expect.soft(got.canvasW > 0 && got.canvasH > 0,
    `canvas backing store is ${got.canvasW}x${got.canvasH}: nothing was painted`).toBe(true);
  expect.soft(got.boxW > 0 && got.boxH > 0, `canvas lays out to ${got.boxW}x${got.boxH}: nothing is on screen`)
    .toBe(true);
  expect.soft(got.layerText, 'text layer').toContain(pdfFixtureText);
  // Both buttons exist enabled in the static shell; setPos disabling both is
  // the paging control's correct one-page state and only the renderer sets it.
  expect.soft({ prev: got.prevDisabled, next: got.nextDisabled }, 'paging disabled on a 1-page doc')
    .toEqual({ prev: true, next: true });

  // The page counter doubles as a copy-link control wired only after render.
  // Both clipboard outcomes end in a flash, so the .on class is deterministic
  // while a dead handler times out here instead of passing vacuously.
  await page.locator('#pos').click();
  await expect(page.locator('#copied')).toHaveClass(/(^|\s)on(\s|$)/);
  const flashed = await page.locator('#copied').evaluate((e) => e.textContent);
  await shot('copy-flash');

  expect.soft(['link copied', 'link is in the address bar'], 'copy flash is one of the shell\'s two confirmations')
    .toContain(flashed);

  pageErrors.expectNone();
});
