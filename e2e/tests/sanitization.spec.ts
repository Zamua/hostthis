import type { Page } from '@playwright/test';
import { test, expect, type Paste, type Server } from '../fixtures';

// fixtureEnd closes every markdown fixture. addHeadingIds in md.js slugs the
// heading text into an id, so waiting on it signals that the WHOLE document
// rendered rather than just its first block.
const fixtureEnd = '\n\n## fixture end\n';

// fixtureEndSel is the id that heading becomes.
const fixtureEndSel = '#fixture-end';

// decoyImageSrc is the src on every broken-image payload: the load has to fail
// for an onerror handler to be worth stripping. Spelled out rather than the
// classic bare "x" so the URL it resolves to can prefix no real slug, which
// would silence that paste's errors along with this 404.
const decoyImageSrc = 'no-such-image.png';

// benignHref proves a link survived. Never fetched: only its rendered href is
// read, and it is not among the anchors the test clicks.
const benignHref = 'https://example.com/safe';

// settleGraceMs is how long a page gets after its images resolve. Asserting a
// sentinel is UNSET is only meaningful once the payload has had its chance;
// the control case runs the same wait and does set its sentinels, which is
// what fixes this window as long enough.
const settleGraceMs = 750;

// controlHTML carries the payload markup on an html paste, which hostthis
// serves verbatim by design (docs/SPEC.md "HTML sandboxing"). It exists to
// prove the payloads, the browser, and the sentinel reads all work, so that
// "the sentinel is unset" on the markdown pages is evidence rather than a
// check that could never have fired.
//
// Production bounds an html paste with one origin per paste. Path mode does
// not model that and nothing here asserts it.
const controlHTML = `<!doctype html>
<html lang="en"><head><meta charset="utf-8"><title>sanitizer control</title></head>
<body>
<p id="control-end">control fixture rendered</p>
<img src="${decoyImageSrc}" onerror="window.__pwnedImg='img-onerror'">
<script>window.__pwned='inline-script'</script>
</body></html>
`;

// sanitizerCases are markdown pastes carrying the payloads a sanitizer
// regression would let through. Markdown is the only kind with a sanitizer to
// regress: an html paste is served as itself, and every other kind renders its
// bytes as text rather than as markup.
const sanitizerCases: { label: string; md: string }[] = [
  {
    label: 'script-tag',
    md: '# sanitizer: script tag\n\n' + "<script>window.__pwned='inline-script'</script>" + fixtureEnd,
  },
  {
    label: 'img-onerror',
    md:
      '# sanitizer: img onerror\n\n' +
      `<img src="${decoyImageSrc}" onerror="window.__pwnedImg='img-onerror'">` +
      fixtureEnd,
  },
  {
    label: 'javascript-url',
    md:
      '# sanitizer: javascript url\n\n' +
      "[payload](javascript:window.__pwned='markdown-link')\n\n" +
      `[benign](${benignHref})` +
      fixtureEnd,
  },
  {
    label: 'raw-html-block',
    md:
      '# sanitizer: raw html block\n\n' +
      `<div onclick="window.__pwned='onclick'" onmouseover="window.__pwned='onmouseover'">` + '\n' +
      `  <iframe src="javascript:window.__pwned='iframe'"></iframe>` + '\n' +
      `  <a href="javascript:window.__pwned='anchor'">payload link</a>` + '\n' +
      `  <form action="javascript:window.__pwned='form'"><button>go</button></form>` + '\n' +
      `</div>` +
      fixtureEnd,
  },
  {
    label: 'svg-script',
    md:
      '# sanitizer: svg script\n\n' +
      `<svg xmlns="http://www.w3.org/2000/svg" width="80" height="80">` + '\n' +
      `  <script>window.__pwned='svg-script'</script>` + '\n' +
      `  <animate onbegin="window.__pwned='svg-animate'" attributeName="x" dur="1s"></animate>` + '\n' +
      `  <circle cx="40" cy="40" r="30" fill="teal"></circle>` + '\n' +
      `</svg>` +
      fixtureEnd,
  },
];

// Pins the one renderer property whose regression is a vulnerability rather
// than a cosmetic bug: anonymous markdown is rendered as HTML on hostthis' own
// origin, so a marked or DOMPurify bump that stops stripping has to fail here.
test('sanitization', async ({ server, page, pageErrors, shot }) => {
  // Playwright dismisses an unhandled dialog silently; a payload that reached
  // alert() must still show up as a failure.
  const dialogs: string[] = [];
  page.on('dialog', async (d) => {
    dialogs.push(`${d.type()}: ${d.message()} at ${page.url()}`);
    await d.dismiss();
  });

  // The decoy image's 404 is the fixture doing its job. Scoped to that exact
  // URL, so every other missing subresource still fails the page.
  pageErrors.ignore(server.baseURL + '/p/' + decoyImageSrc);

  // Every paste is uploaded up front, so the finalizer works on the later
  // ones while the browser is busy with the earlier.
  const control = server.upload(controlHTML, { type: 'html' });
  const pastes = sanitizerCases.map((c) => server.upload(c.md, { type: 'md' }));

  await page.goto(control.url);
  await expect(page.locator('#control-end'), 'control page never rendered').toBeVisible();
  await waitSettled(page);
  await shot('control-executes');
  const ctl = await probeDOM(page, null);
  // What this control does and does NOT license. An html paste is streamed
  // with no CSP header, so it proves the sentinel MECHANISM works: an
  // unsanitized payload on this origin really does set these globals. It does
  // NOT license the sentinels on the markdown cases below, which are served
  // under the shell's `script-src 'self'`. There an inline handler is blocked
  // by CSP whether or not DOMPurify ran, so the sentinel cannot fire even with
  // the sanitizer removed outright. The DOM-presence checks in assertSanitized
  // are the load-bearing assertions for those pages; do not drop them as
  // redundant on the strength of this control.
  expect(
    [ctl.sentinel, ctl.imgSentinel],
    'control did not execute, so the sentinel mechanism is broken',
  ).toEqual(['inline-script', 'img-onerror']);
  expect(ctl.scripts, 'control kept no <script>, so the detectors below prove nothing').toBeGreaterThan(0);
  expect(ctl.eventAttrs.length, 'control kept no event attributes, so the detectors below prove nothing')
    .toBeGreaterThan(0);
  expect.soft(pageErrors.list(), 'page reported errors').toEqual([]);

  for (const [i, c] of sanitizerCases.entries()) {
    const before = pageErrors.list().length;
    await page.goto(pastes[i].url);
    try {
      await page.locator(fixtureEndSel).waitFor({ state: 'visible', timeout: 30_000 });
      // A neutered javascript: href leaves an anchor carrying no href at all.
      // Clicking it turns "the attribute is gone" into "the link is inert",
      // which is the property that matters.
      await page.evaluate(() => {
        document.querySelectorAll<HTMLAnchorElement>('#content a:not([href])').forEach((a) => a.click());
      });
    } catch (e) {
      expect.soft(false, `${c.label}: page never rendered: ${e}`).toBe(true);
      continue;
    }
    await waitSettled(page);
    await shot(c.label);
    assertSanitized(c.label, await probeDOM(page, 'content'));
    expect.soft(pageErrors.list().slice(before), `${c.label}: page reported error(s)`).toEqual([]);
  }

  // The second defense layer, pinned on its own. Sanitization strips the
  // payload; CSP independently blocks anything that survives. Asserting it
  // here means dropping the header is a failure in its own right rather than
  // a silent change that quietly promotes the sentinels above from backstop
  // to load-bearing without anyone noticing.
  await assertShellCSP(pastes[0].url);

  await assertRawIsNotMarkup(server, page, shot, pastes[0]);
  expect.soft(dialogs, 'a payload opened a dialog').toEqual([]);
  pageErrors.expectNone();
});

// assertRawIsNotMarkup covers the second door onto the same untrusted bytes.
// ?raw serves the payload unrendered; served as a type the browser parses as
// markup it executes exactly what the shell path strips. One paste stands for
// all: the raw Content-Type is set by kind, not by content.
async function assertRawIsNotMarkup(
  server: Server,
  page: Page,
  shot: (label: string) => Promise<void>,
  p: Paste,
) {
  // A header read cannot ride the loading page's meta refresh the way a
  // browser assertion does, so the paste has to be ready first.
  await server.waitReady(p);
  const resp = await fetch(p.url + '?raw=1');
  const ct = resp.headers.get('content-type') ?? '';
  const code = resp.status;
  await resp.body?.cancel();
  if (code !== 200) {
    expect.soft(code, 'raw status').toBe(200);
  } else if (ct === '') {
    expect.soft(ct, 'raw carries no Content-Type, leaving the browser to sniff one').not.toBe('');
  } else {
    expect.soft(markupType(ct), `raw served as ${JSON.stringify(ct)}, which a browser parses as markup and executes`)
      .toBe('');
  }

  // What the header claims and what the browser concluded are different
  // facts: a type the browser overrides by sniffing is still a live payload.
  const raw = p.url + '?raw=1';
  await page.goto(raw);
  await waitSettled(page);
  await shot('raw-served-as-text');
  const got = await probeDOM(page, null);
  expect.soft(markupType(got.contentType), `raw: browser parsed ${raw} as markup`).toBe('');
  expect.soft([got.sentinel, got.imgSentinel], 'raw: payload executed through ?raw').toEqual([
    'undefined',
    'undefined',
  ]);
  expect.soft(got.scripts, `raw: <script> reached the DOM at ${raw}`).toBe(0);
}

// assertSanitized checks one rendered markdown page against both ways a payload
// survives: it ran, or it is still sitting there waiting for a user gesture.
function assertSanitized(label: string, p: DomProbe) {
  const bad: string[] = [];
  // Backstop only on a shell page: CSP blocks inline execution independently
  // of sanitization, so this cannot fire here even if DOMPurify is removed.
  // It earns its place on the raw path, which carries no CSP.
  if (p.sentinel !== 'undefined' || p.imgSentinel !== 'undefined') {
    bad.push(`payload executed (sentinels ${JSON.stringify(p.sentinel)}/${JSON.stringify(p.imgSentinel)})`);
  }
  // Load-bearing from here down: these are what actually go red when the
  // sanitizer stops stripping.
  if (p.scripts !== 0) bad.push(`${p.scripts} <script> survived`);
  if (p.iframes !== 0) bad.push(`${p.iframes} <iframe> survived`);
  if (p.eventAttrs.length !== 0) bad.push('event handler attributes survived: ' + p.eventAttrs.join(', '));
  if (p.jsURLAttrs.length !== 0) bad.push('javascript: URLs survived: ' + p.jsURLAttrs.join(', '));
  // One fixture carries a benign link. Its href surviving is what stops the
  // javascript: check above from passing merely because no anchor rendered.
  if ('benign' in p.anchors && p.anchors.benign !== benignHref) {
    bad.push(
      `benign link href = ${JSON.stringify(p.anchors.benign)}, want ${JSON.stringify(benignHref)}: ` +
        'link rendering is broken, so the javascript: check proves nothing',
    );
  }
  expect.soft(bad, `${label}: rendered as:\n${p.html}`).toEqual([]);
}

// DomProbe is what one page reports about the markup that reached its DOM.
type DomProbe = {
  // sentinel and imgSentinel read "undefined" unless a payload ran. Two names,
  // so the synchronous path (an inline script) and the asynchronous one (an
  // image error handler) are provable apart.
  sentinel: string;
  imgSentinel: string;
  contentType: string;
  scripts: number;
  iframes: number;
  // eventAttrs and jsURLAttrs name surviving attributes as "tag@attr".
  eventAttrs: string[];
  jsURLAttrs: string[];
  // anchors maps a link's text to its rendered href, "" when it has none.
  anchors: Record<string, string>;
  html: string;
};

// probeDOM reads the whole probe in one round trip. rootId names the region to
// inspect, null for the body, so the same probe covers the shell's #content and
// a raw page's body.
//
// The javascript: match tolerates leading whitespace and control characters
// because a URL's scheme is read with those stripped, which is how the payload
// hides from a naive prefix check.
function probeDOM(page: Page, rootId: string | null): Promise<DomProbe> {
  return page.evaluate((id) => {
    const root = id === null ? document.body : document.getElementById(id)!;
    const eventAttrs: string[] = [];
    const jsURLAttrs: string[] = [];
    const anchors: Record<string, string> = {};
    for (const el of Array.from(root.querySelectorAll('*'))) {
      for (const a of Array.from(el.attributes)) {
        const where = el.tagName.toLowerCase() + '@' + a.name;
        if (/^on/i.test(a.name)) eventAttrs.push(where);
        if (/^[\s\x00-\x1F]*javascript:/i.test(a.value)) jsURLAttrs.push(where);
      }
    }
    for (const a of Array.from(root.querySelectorAll('a'))) {
      anchors[a.textContent!.trim()] = a.getAttribute('href') || '';
    }
    const w = window as unknown as { __pwned?: unknown; __pwnedImg?: unknown };
    return {
      sentinel: String(w.__pwned),
      imgSentinel: String(w.__pwnedImg),
      contentType: document.contentType,
      scripts: root.querySelectorAll('script').length,
      iframes: root.querySelectorAll('iframe').length,
      eventAttrs,
      jsURLAttrs,
      anchors,
      html: root.innerHTML,
    };
  }, rootId);
}

// waitSettled blocks until every image on the page has resolved, then holds for
// settleGraceMs. Without it, "the sentinel is unset" could mean only that the
// assertion ran first.
async function waitSettled(page: Page) {
  await expect
    .poll(() => page.evaluate(() => Array.from(document.images).every((i) => i.complete)), {
      message: 'wait for page to settle',
      timeout: 15_000,
    })
    .toBe(true);
  await page.waitForTimeout(settleGraceMs);
}

// markupTypes are the response types a browser parses as markup and runs script
// from.
const markupTypes = ['text/html', 'application/xhtml+xml', 'image/svg+xml', 'text/xml', 'application/xml'];

// markupType returns the matching markup type, or "" when the type is inert.
function markupType(contentType: string) {
  const ct = contentType.trim().toLowerCase();
  return markupTypes.find((m) => ct.startsWith(m)) ?? '';
}

// assertShellCSP pins that a rendered shell is served with a script-src policy
// that forbids inline execution. Paired with the sanitizer checks so the two
// layers are verified separately rather than through one end-to-end signal that
// cannot say which of them held.
async function assertShellCSP(url: string) {
  const resp = await fetch(url);
  await resp.body?.cancel();
  const csp = resp.headers.get('content-security-policy') ?? '';
  if (csp === '') {
    expect.soft(csp, 'shell served with no Content-Security-Policy; the sanitizer is now the only layer').not.toBe('');
    return;
  }
  // Read the script-src directive specifically. A substring search over the
  // whole policy cannot tell the directives apart, and style-src legitimately
  // carries unsafe-inline; only script-src decides whether an injected handler
  // can run.
  let scriptSrc = '';
  for (const d of csp.split(';')) {
    const f = d.trim().split(/\s+/).filter(Boolean);
    if (f.length > 0 && f[0] === 'script-src') scriptSrc = f.slice(1).join(' ');
  }
  if (scriptSrc === '') {
    expect.soft(scriptSrc, `CSP ${JSON.stringify(csp)} names no script-src, so inline execution is unconstrained`)
      .not.toBe('');
    return;
  }
  for (const unsafe of ["'unsafe-inline'", "'unsafe-eval'"]) {
    expect.soft(scriptSrc, `script-src allows ${unsafe}, which defeats the layer it exists to provide`)
      .not.toContain(unsafe);
  }
}
