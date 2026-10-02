import { test, expect } from '../fixtures';
import {
  atFolder, clearSearch, clickSel, deepBaseFiles, deepDir, deepFile, deepNames, drill, get,
  headingReached, knowledgeBaseFiles, readKBLinkColors, readKBPanes, readKBSearch, readKBTarget,
  readKBView, settled, treeLeft, treeRight, wideContentFiles,
} from './knowledgebase.helpers';

const phone = { width: 390, height: 844 };

// The knowledge base shell browses a directory of documents: it opens the
// root's README, navigates by relative link, by sidebar and by direct URL, and
// resolves a heading fragment on load and on hashchange.
test('knowledge base', async ({ server, page, pageErrors, shot }) => {
  const paste = server.uploadDir(knowledgeBaseFiles, { name: 'knowledge base' });
  await page.goto(paste.url);

  // -- the root renders README.md, the sidebar shows the root level ---------
  await expect(page.locator('#kb-tree[data-tree-ready="1"]')).toBeVisible();
  await expect(page.locator(settled('README.md'))).toBeVisible();
  const root = await readKBView(page);
  await shot('root');

  expect.soft(root.heading, "root h1, want the README's heading").toBe('Knowledge base root');
  expect.soft(root.docPath).toBe('README.md');
  // The deploy prints the base with no trailing slash in path mode, which
  // would resolve a relative link one level ABOVE the base.
  expect.soft(root.path.endsWith(`/p/${paste.slug}/`), `root URL ${root.path} normalised to end in /p/${paste.slug}/`).toBe(true);
  // The sidebar shows ONE level: the root's own children, never the whole
  // tree, so nothing below the top level is listed yet.
  expect.soft(root.filePaths, 'sidebar files at the root').toEqual(['README.md']);
  expect.soft(root.dirPaths, 'sidebar folders at the root').toEqual(['assets', 'guides', 'notes', 'reference']);
  expect.soft([root.here, root.hasParent], 'the root level names no folder and offers no parent').toEqual(['', false]);
  expect.soft(root.treeFiles, "tree data-files, the whole base's count").toBe('6');
  expect.soft(root.ellipsis, `a two-segment trail collapsed: ${root.crumbs}`).toBe(false);
  expect.soft(root.crumbs).toEqual(['root:root', 'file:README.md']);
  expect.soft(root.tocLinks, "root TOC, the README's headings").toEqual(['knowledge-base-root', 'overview', 'what-is-inside']);

  // -- a relative link in the ROOT document opens in the shell --------------
  // The case a missing trailing slash breaks: the href is relative to the
  // base, so it only resolves inside it when the URL names the base itself.
  // The marker dies with the document, so its survival is what separates
  // a pushState swap from a reload that happens to land on the same URL.
  await page.evaluate(() => ((window as unknown as { __kbMarker: string }).__kbMarker = 'alive'));
  await clickSel(page, '#kb-doc a[href="guides/setup.md"]');
  await expect(page.locator(settled('guides/setup.md'))).toBeVisible();
  const rel = await readKBView(page);
  await shot('document');

  expect.soft(rel.marker, 'a relative link from the root left the shell').toBe('alive');
  expect.soft(rel.historyLength, 'history length grows, so the navigation did pushState').toBeGreaterThan(root.historyLength);
  expect.soft(rel.path.endsWith('/guides/setup.md'), `URL ${rel.path} ends in /guides/setup.md`).toBe(true);
  expect.soft(rel.heading).toBe('Setup');
  // The sidebar follows the reader into the opened document's folder.
  expect.soft(rel.here, "sidebar folder, the opened document's own").toBe('guides');
  expect.soft(rel.current, 'sidebar current entry').toEqual(['guides/setup.md:page']);
  expect.soft(rel.crumbs).toEqual(['root:root', 'dir:guides', 'file:setup.md']);
  expect.soft(rel.tocLinks, "TOC, this document's headings").toEqual([
    'setup', 'install-the-daemon', 'configure-the-listener', 'verify-the-install',
  ]);

  // -- relative links between documents, down and back up -------------------
  await clickSel(page, '#kb-doc a[href="advanced/tuning.md"]');
  await expect(page.locator(settled('guides/advanced/tuning.md'))).toBeVisible();
  const down = await readKBView(page);
  expect.soft(down.heading, 'relative link into a subdirectory').toBe('Tuning');
  expect.soft(down.marker, 'following a relative link reloaded the page').toBe('alive');
  expect.soft(down.here, 'sidebar folder').toBe('guides/advanced');

  await clickSel(page, '#kb-doc a[href="../setup.md"]');
  await expect(page.locator(settled('guides/setup.md'))).toBeVisible();
  const up = await readKBView(page);
  expect.soft(up.heading, 'relative link out of a subdirectory').toBe('Setup');

  // -- the sidebar navigates folders without touching the document ----------
  await clickSel(page, '#kb-tree button.kb-up[data-parent=""]');
  await expect(page.locator(atFolder(''))).toBeVisible();
  const upOne = await readKBView(page);
  expect.soft([upOne.here, upOne.hasParent], 'the parent control lands on the root').toEqual(['', false]);
  expect.soft([upOne.docPath, upOne.marker], 'moving the sidebar changed the document').toEqual(['guides/setup.md', 'alive']);
  const atRoot = upOne;
  expect.soft(atRoot.dirPaths, 'sidebar folders back at the root').toEqual(['assets', 'guides', 'notes', 'reference']);

  await clickSel(page, '#kb-tree button.kb-dir[data-dir="notes"]');
  await expect(page.locator(atFolder('notes'))).toBeVisible();
  const inNotes = await readKBView(page);
  expect.soft([inNotes.here, inNotes.parent], 'entering notes/, want notes and the root').toEqual(['notes', '']);
  expect.soft(inNotes.filePaths, 'notes/ lists').toEqual(['notes/page.html']);
  expect.soft(inNotes.docPath, 'entering a folder changed the document').toBe('guides/setup.md');

  // -- a non-markdown entry links out instead of rendering in the shell -----
  const ext = await page.evaluate(() => {
    const links = document.querySelectorAll('#kb-tree a.kb-ext[data-external="1"]');
    const a = document.querySelector<HTMLAnchorElement>('#kb-tree a.kb-file[data-path="notes/page.html"]')!;
    return { count: links.length, target: a.target, rel: a.rel, external: a.dataset.external || '', href: a.href };
  });
  expect.soft(ext.count, 'entries in notes/ marked external, want 1 (the html file)').toBe(1);
  expect.soft(
    ext.target === '_blank' && ext.external === '1' && ext.rel.includes('noopener'),
    `notes/page.html entry ${JSON.stringify(ext)} is an external link opening in a new tab`,
  ).toBe(true);
  // The other half of "does not render inside the shell": its own URL serves
  // the file's bytes, never the shell that would render them.
  const raw = await get(ext.href);
  expect.soft(
    raw.body.includes('served raw') && !raw.body.includes('/_hostthis/kb.js'),
    `GET ${ext.href} served ${JSON.stringify(raw.body)} (${raw.contentType}), want the raw file rather than the shell`,
  ).toBe(true);

  // -- a direct load of the same nested path renders the same document ------
  await page.goto(paste.url + '/guides/setup.md');
  await expect(page.locator(settled('guides/setup.md'))).toBeVisible();
  const direct = await readKBView(page);
  expect.soft(direct.marker, 'the direct load did not reload, so it proves nothing about a typed URL').toBe('');
  expect.soft(
    { heading: direct.heading, crumbs: direct.crumbs, tocLinks: direct.tocLinks, current: direct.current, here: direct.here },
    'a typed URL renders the same as clicking to it',
  ).toEqual({ heading: rel.heading, crumbs: rel.crumbs, tocLinks: rel.tocLinks, current: rel.current, here: rel.here });

  // -- a heading fragment resolves on load, and again on hashchange ---------
  await page.goto(paste.url + '/guides/setup.md#configure-the-listener');
  await expect(page.locator(settled('guides/setup.md'))).toBeVisible();
  // The browser resolved the fragment against an empty document at parse
  // time, so the shell resolving it is an event later than the paint.
  await expect.poll(() => headingReached(page, 'configure-the-listener'), { timeout: 10_000 }).toBe(true);
  const onLoad = await readKBTarget(page);
  await shot('deep-link');
  expect.soft([onLoad.found, onLoad.inView], `fragment #configure-the-listener resolves into the reading pane: ${JSON.stringify(onLoad)}`).toEqual([true, true]);

  await page.evaluate(() => (location.hash = '#verify-the-install'));
  await expect.poll(() => headingReached(page, 'verify-the-install'), { timeout: 10_000 }).toBe(true);
  const onChange = await readKBTarget(page);
  expect.soft([onChange.found, onChange.inView], `hashchange to #verify-the-install resolves: ${JSON.stringify(onChange)}`).toEqual([true, true]);

  // -- scroll-spy marks the heading the reader is under ---------------------
  // Anchored at the top of the pane: the heading the reader has reached, not
  // whichever one is merely on screen.
  await page.evaluate(() => document.getElementById('install-the-daemon')!.scrollIntoView({ block: 'start' }));
  await expect
    .poll(
      () => page.evaluate(() => {
        const a = document.querySelector<HTMLElement>("#kb-toc-list a.kb-toc-link[aria-current='true']");
        return !!a && a.dataset.targetId === 'install-the-daemon';
      }),
      { timeout: 10_000 },
    )
    .toBe(true);
  const spied = await readKBView(page);
  expect.soft(spied.tocActive, 'scroll-spy active entry').toEqual(['install-the-daemon']);

  pageErrors.expectNone();
});

// Search matches a term that occurs only in a document's body, opens the file
// it found, matches on path alone, and reports what the index covers.
test('knowledge base search', async ({ server, page, pageErrors, shot }) => {
  const paste = server.uploadDir(knowledgeBaseFiles, { name: 'knowledge base search' });
  await page.goto(paste.url);
  const search = page.locator('#kb-search');

  await expect(page.locator(settled('README.md'))).toBeVisible();
  // Bodies are indexed in the background, so a search before this is a race
  // against the fetch rather than a test of matching.
  await expect(page.locator('body[data-kb-index="complete"]')).toBeAttached();
  const idle = await readKBSearch(page);
  expect.soft([idle.indexed, idle.total], 'index note coverage, want 4 of 4 markdown files').toEqual(['4', '4']);
  // A complete index has nothing to warn about, so the note stays out of the
  // way; a partial one is what has to speak up.
  expect.soft(idle.noteHidden, `index note is visible on a complete index: ${idle.noteText}`).toBe(true);
  expect.soft(idle.noteText, 'the hidden note still carries text').toBe('');

  await search.pressSequentially('quokka');
  await expect(page.locator('#kb-results .kb-hit').first()).toBeVisible();
  const text = await readKBSearch(page);
  await shot('search-results');
  expect(text.hits, 'searching a body-only term').toHaveLength(1);
  expect.soft([text.hits[0].path, text.hits[0].kind], 'body hit, want a text match in guides/setup.md').toEqual(['guides/setup.md', 'text']);
  expect.soft(text.hits[0].text, 'hit snippet shows the match').toContain('quokka');
  expect.soft(text.state, 'index state').toBe('complete');

  // -- a term matches whatever its case ------------------------------------
  // The index keeps one copy of each document, so matching is the pattern's
  // job rather than a lowercased twin's.
  await clearSearch(page);
  await search.pressSequentially('QUOKKA');
  await expect(page.locator('#kb-results .kb-hit').first()).toBeVisible();
  const upper = await readKBSearch(page);
  expect.soft(upper.hits.map((h) => h.path), 'an upper-case query, want the same one text hit').toEqual(['guides/setup.md']);

  // -- the hit opens the file it named --------------------------------------
  await clearSearch(page);
  await search.pressSequentially('quokka');
  await expect(page.locator('#kb-hits .kb-hit').first()).toBeVisible();
  await clickSel(page, '#kb-hits .kb-hit');
  await expect(page.locator(settled('guides/setup.md'))).toBeVisible();
  const opened = await readKBView(page);
  expect.soft(opened.heading, 'the hit opened').toBe('Setup');
  expect.soft(opened.path.endsWith('/guides/setup.md'), `URL after opening the hit = ${opened.path}`).toBe(true);
  // A hit reaches any file at any depth; the sidebar follows it there, which
  // is what makes navigating one level at a time enough.
  expect.soft(opened.here, 'sidebar folder after opening a hit').toBe('guides');

  // -- a path-only term still matches, because paths never need the index ---
  await clearSearch(page);
  await search.pressSequentially('logo');
  await expect(page.locator('#kb-hits .kb-hit[data-kind="path"]').first()).toBeVisible();
  const byPath = await readKBSearch(page);
  expect(byPath.hits.map((h) => h.path), 'path search, want the one png').toEqual(['assets/logo.png']);
  // A binary file is not rendered by the shell, so its hit leaves too.
  expect.soft(byPath.hits[0].external, 'path hit for a binary file is an external link').toBe('1');

  await clearSearch(page);
  await search.pressSequentially('nosuchterm');
  await expect(page.locator('#kb-results-empty')).toBeVisible();
  const none = await readKBSearch(page);
  expect.soft([none.hits.length, none.emptyHidden], 'a term matching nothing, want no hits and the empty note shown').toEqual([0, false]);

  pageErrors.expectNone();
});

// A partial index names what actually stopped it. A document that could not be
// read is not the size bound, and this base is nowhere near either limit.
test('knowledge base partial index', async ({ server, page, pageErrors, shot }) => {
  const paste = server.uploadDir(knowledgeBaseFiles, { name: 'knowledge base partial index' });
  // Fails ONE document's raw fetch and nothing else, so the index falls short
  // for a reason that is not the size bound.
  await page.addInitScript(() => {
    const orig = window.fetch;
    window.fetch = function (this: unknown, ...args: Parameters<typeof fetch>) {
      const input = args[0];
      const url = typeof input === 'string' ? input : (input && (input as Request).url) || '';
      if (url.indexOf('guides/setup.md?raw=1') >= 0) {
        return Promise.reject(new Error('blocked by the e2e fixture'));
      }
      return orig.apply(this, args);
    } as typeof fetch;
  });
  await page.goto(paste.url);

  await expect(page.locator(settled('README.md'))).toBeVisible();
  await expect(page.locator('body[data-kb-index="partial"]')).toBeAttached();
  // The note lives in the results panel, which a query opens.
  await page.locator('#kb-search').pressSequentially('setup');
  await expect(page.locator('#kb-index-note')).toBeVisible();
  const got = await readKBSearch(page);
  await shot('partial-index');

  expect.soft([got.indexed, got.total], 'index coverage, want 3 of 4 with one unreadable').toEqual(['3', '4']);
  expect.soft(got.noteText, 'the note names the failed read').toContain('1 document(s) could not be read');
  expect.soft(got.noteText, 'the note blames the size bound for a failed read').not.toContain('MiB of markdown');
  pageErrors.expectNone();
});

// A base holding no markdown at all renders the shell's generated listing
// rather than an error or a blank page.
test('knowledge base without markdown', async ({ server, page, pageErrors, shot }) => {
  const paste = server.uploadDir(
    {
      'notes/page.html': '<!doctype html><title>raw page</title><p>served raw</p>',
      'data/values.json': '{"answer":42}',
      'assets/logo.png': '\x89PNG\r\n\x1a\n\x00not really a png',
    },
    { name: 'knowledge base listing' },
  );
  await page.goto(paste.url);

  await expect(page.locator('#kb-tree[data-tree-ready="1"]')).toBeVisible();
  await expect(page.locator('#kb-listing')).toBeVisible();
  const got = await page.evaluate(() => {
    const doc = document.getElementById('kb-doc')!;
    const items = Array.from(document.querySelectorAll<HTMLAnchorElement>('#kb-listing a'));
    return {
      heading: doc.querySelector('h1')!.textContent!.trim(),
      listing: items.map((a) => a.dataset.path!).sort(),
      external: items.filter((a) => a.dataset.external === '1' && a.target === '_blank').length,
      error: !!document.getElementById('kb-error'),
      docText: doc.textContent!.trim().slice(0, 80),
      tocHidden: document.getElementById('kb-toc')!.hidden,
    };
  });
  await shot('listing');

  expect.soft(got.error, `a base with no markdown rendered an error panel: ${got.docText}`).toBe(false);
  expect.soft(got.heading, 'listing heading').toBe('Files');
  const want = ['assets/logo.png', 'data/values.json', 'notes/page.html'];
  expect.soft(got.listing, 'listing, want every file').toEqual(want);
  // Nothing here renders in the shell, so every entry leaves to its own URL.
  expect.soft(got.external, 'listing entries that link out, want all of them').toBe(want.length);
  expect.soft(got.tocHidden, 'the table of contents is shown for a document with no headings').toBe(true);

  pageErrors.expectNone();
});

// Content wider than the reading pane is CONTAINED by it: the table and the
// code block scroll inside their own boxes and the page never scrolls sideways.
test('knowledge base wide content', async ({ server, page, pageErrors, shot }) => {
  const paste = server.uploadDir(wideContentFiles, { name: 'knowledge base wide content' });
  await page.goto(paste.url);

  const readWide = () =>
    page.evaluate(() => {
      const t = document.querySelector('#kb-doc table');
      const p = document.querySelector('#kb-doc pre');
      return {
        pageWidth: document.documentElement.scrollWidth,
        innerWidth: window.innerWidth,
        tableScroll: !!t && t.scrollWidth > t.clientWidth,
        preScroll: !!p && p.scrollWidth > p.clientWidth,
      };
    });

  await expect(page.locator(settled('README.md'))).toBeVisible();
  await expect(page.locator('#kb-doc table')).toBeVisible();
  const desktop = await readWide();
  await shot('wide-desktop');
  expect.soft(desktop.pageWidth, `the page scrolls sideways at ${desktop.innerWidth}px`).toBeLessThanOrEqual(desktop.innerWidth);
  expect.soft([desktop.tableScroll, desktop.preScroll], 'wide content scrolls within its own box').toEqual([true, true]);

  await page.setViewportSize(phone);
  await expect.poll(() => page.evaluate(() => window.innerWidth === 390), { timeout: 10_000 }).toBe(true);
  const onPhone = await readWide();
  await shot('wide-phone');
  expect.soft(onPhone.pageWidth, `the page scrolls sideways at 390px (innerWidth ${onPhone.innerWidth})`).toBeLessThanOrEqual(onPhone.innerWidth);
  expect.soft([onPhone.tableScroll, onPhone.preScroll], 'wide content scrolls within its own box on a phone').toEqual([true, true]);

  pageErrors.expectNone();
});

// Ten folders deep, the sidebar still shows one level and the breadcrumb trail
// still fits: the walk down, the file at the bottom, and the walk back up by
// the parent control and by a crumb.
test('knowledge base deep navigation', async ({ server, page, pageErrors, shot }) => {
  const paste = server.uploadDir(deepBaseFiles, { name: 'knowledge base deep' });
  await page.goto(paste.url);

  await expect(page.locator('#kb-tree[data-tree-ready="1"]')).toBeVisible();
  await expect(page.locator(settled('README.md'))).toBeVisible();
  const root = await readKBView(page);
  await shot('sidebar-root');
  expect.soft(root.dirPaths, 'the root level lists only the top folder').toEqual([deepNames[0]]);
  expect.soft(root.ellipsis, `the root document's two-segment trail collapsed: ${root.crumbs}`).toBe(false);

  await drill(page, deepNames);
  const deep = await readKBView(page);
  expect(deep.here, 'the sidebar after drilling ten levels').toBe(deepDir);
  expect.soft(deep.parent, 'the parent control points at the ninth level').toBe(deepNames.slice(0, -1).join('/'));
  expect.soft(deep.filePaths, 'the deepest level lists').toEqual([deepDir + '/level.md', deepDir + '/notes.md']);
  expect.soft(deep.dirPaths, 'the deepest level lists no folders').toEqual([]);
  // Ten levels of sidebar navigation, and the document never moved.
  expect.soft(deep.docPath, 'walking the sidebar changed the document').toBe('README.md');

  await clickSel(page, `#kb-tree a.kb-file[data-path="${deepFile}"]`);
  await expect(page.locator(settled(deepFile))).toBeVisible();
  const opened = await readKBView(page);
  await shot('sidebar-depth-10');
  expect.soft(opened.heading, 'the deepest document rendered').toBe('Level 10');
  expect.soft(opened.current, 'the current file is marked at depth ten').toEqual([deepFile + ':page']);
  // The trail is one line: the middle gave way, the ends did not.
  expect.soft(opened.crumbsFit, `the breadcrumb trail overflows its bar: ${opened.crumbs}`).toBe(true);
  expect.soft(opened.ellipsis, `a ten-segment trail did not collapse: ${opened.crumbs}`).toBe(true);
  expect.soft(opened.crumbs[0], 'the trail keeps its first segment').toBe('root:root');
  expect.soft(opened.crumbs[opened.crumbs.length - 1], "the trail's last segment, want the current file").toBe('file:level.md');

  // -- the ellipsis expands the whole path, and collapses again -------------
  await clickSel(page, '#kb-crumbs .kb-crumb-more');
  await expect(page.locator('#kb-crumbs.expanded')).toBeVisible();
  const expanded = await readKBView(page);
  await shot('crumbs-expanded');
  // root + ten folders + the file + the control itself.
  expect.soft(expanded.crumbs, 'expanded trail shows every segment').toHaveLength(deepNames.length + 3);
  expect.soft(expanded.expanded, "the control's aria-expanded").toBe('true');
  expect.soft(expanded.pageFits, 'the expanded trail made the page scroll sideways rather than wrap').toBe(true);

  await clickSel(page, '#kb-crumbs .kb-crumb-more');
  const recollapsed = await readKBView(page);
  expect.soft(
    [recollapsed.expanded, recollapsed.crumbsFit],
    `a second click collapses the trail (${recollapsed.crumbs})`,
  ).toEqual(['false', true]);

  // Opening another document collapses an expanded trail.
  await clickSel(page, '#kb-crumbs .kb-crumb-more');
  await expect(page.locator('#kb-crumbs.expanded')).toBeVisible();
  await clickSel(page, `#kb-tree a.kb-file[data-path="${deepDir}/notes.md"]`);
  await expect(page.locator(settled(deepDir + '/notes.md'))).toBeVisible();
  const switched = await readKBView(page);
  expect.soft([switched.expanded, switched.crumbsFit], 'the trail stayed expanded across a document change').toEqual(['false', true]);

  // -- back up, by the parent control and by a crumb ------------------------
  const ninth = deepNames.slice(0, -1).join('/');
  await clickSel(page, `#kb-tree button.kb-up[data-parent="${ninth}"]`);
  await expect(page.locator(atFolder(ninth))).toBeVisible();
  const upOne = await readKBView(page);
  expect.soft(upOne.here, 'the parent control landed at').toBe(ninth);
  expect.soft(upOne.docPath, 'going up changed the document').toBe(deepDir + '/notes.md');

  await clickSel(page, '#kb-crumbs button.kb-crumb[data-crumb-kind="dir"]');
  await expect(page.locator(atFolder(deepNames[0]))).toBeVisible();
  const byCrumb = await readKBView(page);
  expect.soft(byCrumb.here, 'the first folder crumb moved the sidebar to').toBe(deepNames[0]);
  expect.soft(byCrumb.docPath, 'a crumb click changed the document').toBe(deepDir + '/notes.md');

  pageErrors.expectNone();
});

test.describe('on a phone', () => {
  test.use({ viewport: phone });

  // The same depth on a phone: the drawer, the walk down, and a trail that
  // collapses to one line and expands without widening the page.
  test('knowledge base deep navigation on a phone', async ({ server, page, pageErrors, shot }) => {
    const paste = server.uploadDir(deepBaseFiles, { name: 'knowledge base deep phone' });
    await page.goto(paste.url);

    await expect(page.locator(settled('README.md'))).toBeVisible();
    await page.locator('#kb-files-toggle').click();
    await expect.poll(() => treeLeft(page), { timeout: 10_000 }).toBeGreaterThanOrEqual(0);
    await drill(page, deepNames);
    const deep = await readKBView(page);
    await shot('sidebar-phone');
    expect(deep.here, 'the phone sidebar after ten levels').toBe(deepDir);
    // A folder is sidebar navigation, so the drawer stays open for the next tap.
    expect.soft(deep.pageFits, 'walking the sidebar on a phone made the page scroll sideways').toBe(true);

    await clickSel(page, `#kb-tree a.kb-file[data-path="${deepFile}"]`);
    await expect(page.locator(settled(deepFile))).toBeVisible();
    // The drawer slides shut behind the opened file; reading or photographing
    // mid-slide captures a half-open sidebar.
    await expect.poll(() => treeRight(page), { timeout: 10_000 }).toBeLessThanOrEqual(0);
    const opened = await readKBView(page);
    await shot('phone-crumbs-collapsed');
    expect.soft(opened.pageFits, `the page scrolls sideways at 390px with a ten-deep document open: ${opened.crumbs}`).toBe(true);
    expect.soft([opened.crumbsFit, opened.ellipsis], `the trail fits one phone-width line (${opened.crumbs})`).toEqual([true, true]);
    expect.soft(
      [opened.crumbs[0], opened.crumbs[opened.crumbs.length - 1]],
      'the collapsed trail keeps both ends',
    ).toEqual(['root:root', 'file:level.md']);

    await clickSel(page, '#kb-crumbs .kb-crumb-more');
    await expect(page.locator('#kb-crumbs.expanded')).toBeVisible();
    const expanded = await readKBView(page);
    await shot('phone-crumbs-expanded');
    expect.soft(expanded.crumbs, 'the expanded trail shows every segment').toHaveLength(deepNames.length + 3);
    expect.soft(expanded.pageFits, 'the expanded trail widened the page instead of wrapping').toBe(true);

    pageErrors.expectNone();
  });

  // On a phone both sidebars are drawers over the document: closed until asked
  // for, closed again by opening a file, and never adding scrollable width.
  test('knowledge base on a phone', async ({ server, page, pageErrors, shot }) => {
    const paste = server.uploadDir(knowledgeBaseFiles, { name: 'knowledge base phone' });
    await page.goto(paste.url);

    await expect(page.locator(settled('README.md'))).toBeVisible();
    const closed = await readKBPanes(page);
    await shot('phone-root');
    // Off-canvas rather than removed: the drawer slides in, so it stays laid
    // out and only its position says whether it is showing.
    expect.soft([closed.bodyClasses, closed.scrimDisplay], 'a phone-width page starts with both drawers closed').toEqual(['', 'none']);
    expect.soft(closed.treeRight, `the files drawer is on screen before it was asked for: ${JSON.stringify(closed)}`).toBeLessThanOrEqual(0);
    expect.soft(closed.tocLeft, `the contents drawer is on screen before it was asked for: ${JSON.stringify(closed)}`).toBeGreaterThanOrEqual(closed.width);
    // An off-canvas drawer is parked outside the page, not beside it: unclipped
    // it would add its own width to the page and scroll the phone sideways.
    expect.soft(closed.pageWidth, 'page width with both drawers closed').toBeLessThanOrEqual(closed.width);

    // Each drawer is waited for by its POSITION: the class lands when the
    // transition starts, so polling on it would photograph a half-open drawer
    // and read its transform mid-slide.
    await page.locator('#kb-files-toggle').click();
    await expect.poll(() => treeLeft(page), { timeout: 10_000 }).toBeGreaterThanOrEqual(0);
    const open = await readKBPanes(page);
    await shot('files-drawer');
    expect.soft(open.bodyClasses, 'the files drawer opens over the document').toContain('tree-open');
    expect.soft(open.scrimDisplay, 'the files drawer opens over the document').toBe('block');
    expect.soft(open.pageWidth, 'page width with the files drawer open').toBeLessThanOrEqual(open.width);

    // The sidebar walks folders on a phone the same way, and the drawer stays
    // open across it: a folder is navigation within the drawer, not a choice of
    // document that closes it.
    await clickSel(page, '#kb-tree button.kb-dir[data-dir="guides"]');
    await expect(page.locator(atFolder('guides'))).toBeVisible();
    const drilled = await readKBPanes(page);
    expect.soft(drilled.bodyClasses, 'entering a folder closed the drawer').toContain('tree-open');

    await clickSel(page, '#kb-tree a.kb-file[data-path="guides/setup.md"]');
    await expect(page.locator(settled('guides/setup.md'))).toBeVisible();
    await expect.poll(() => treeRight(page), { timeout: 10_000 }).toBeLessThanOrEqual(0);
    const afterOpenFile = await readKBPanes(page);
    await shot('phone-document');
    // Opening a file closes the drawer: leaving it over the document would hide
    // what the tap just asked for.
    expect.soft([afterOpenFile.bodyClasses, afterOpenFile.scrimDisplay], 'the drawer outlived the file it opened').toEqual(['', 'none']);

    await page.locator('#kb-toc-toggle').click();
    await expect
      .poll(() => page.evaluate(() => document.getElementById('kb-toc')!.getBoundingClientRect().right <= window.innerWidth), { timeout: 10_000 })
      .toBe(true);
    const tocOpen = await readKBPanes(page);
    await shot('toc-drawer');
    expect.soft(tocOpen.bodyClasses, 'the contents drawer opens over the document').toContain('toc-open');
    expect.soft(tocOpen.scrimDisplay, 'the contents drawer opens over the document').toBe('block');
    expect.soft(tocOpen.tocLeft, 'the contents drawer is still off screen').toBeLessThan(tocOpen.width);
    expect.soft(tocOpen.pageWidth, 'page width with the contents drawer open').toBeLessThanOrEqual(tocOpen.width);

    pageErrors.expectNone();
  });
});

// A document's links divide into those that stay in this base and those that
// leave it. The shell must say which before the click: what leaves is marked
// and opens in a new tab, what stays opens in place, and what the sanitizer
// disarmed stops looking like a link at all.
test('knowledge base links', async ({ server, page, shot }) => {
  const paste = server.uploadDir(knowledgeBaseFiles, { name: 'knowledge base links' });
  await page.goto(paste.url);

  await expect(page.locator('#kb-tree[data-tree-ready="1"]')).toBeVisible();
  await expect(page.locator(settled('README.md'))).toBeVisible();
  const view = await readKBView(page);
  const colors = await readKBLinkColors(page);
  await shot('links');

  // href | data-external | target | rel | external mark, in document order.
  expect.soft(view.docLinks, 'document links').toEqual([
    'guides/setup.md||||',
    'reference/api.md||||',
    'https://example.com/docs|1|_blank|noopener noreferrer|mark',
    'notes/page.html|1|_blank|noopener noreferrer|mark',
    'mailto:nobody@example.com|1|||mark',
    '||||',
  ]);

  expect.soft(colors.dead, "a disarmed link renders in the document's own colour").toBe(colors.doc);
  expect.soft(colors.dead, 'a disarmed link renders in the same colour as a live link').not.toBe(colors.live);

  // A link that leaves the base keeps the shell out of it: the click handler
  // skips anything marked external, so the page it is on does not change.
  await page.evaluate(() => ((window as unknown as { __kbMarker: string }).__kbMarker = 'alive'));
  // target="_blank" would open a tab the test does not drive, so the anchor is
  // neutralised first: what is under test is the shell's own handler declining
  // it, not the browser's tab handling.
  await page.evaluate(() => {
    const a = document.querySelector<HTMLAnchorElement>('#kb-doc a[href="https://example.com/docs"]')!;
    a.removeAttribute('target');
    a.addEventListener('click', (ev) => ev.preventDefault(), { once: true });
    a.click();
  });
  const after = await readKBView(page);
  expect.soft([after.docPath, after.marker], 'clicking a link out of the base moved the shell, want it to stay on README.md').toEqual(['README.md', 'alive']);

  // The raw page the document links to is the same bytes the server serves at
  // its own URL, which is why linking out rather than rendering is correct.
  const raw = await get(paste.url + '/notes/page.html');
  expect.soft(raw.body, "linked HTML file serves the file's own bytes").toContain('served raw');
  expect.soft(raw.contentType.startsWith('text/html'), `linked HTML file content-type = ${raw.contentType}`).toBe(true);
});
