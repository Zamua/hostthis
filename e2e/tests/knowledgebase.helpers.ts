import { expect, type Page } from '@playwright/test';

// kbFiller pads a section so the reading pane scrolls, which scroll-spy needs.
const kbFiller = 'Filler text that pads this section so the reading pane scrolls.\n\n'.repeat(8);

// knowledgeBaseFiles is a base with nested directories, a README, several
// headings per document, relative links both down into a subdirectory and back
// up out of one, an HTML file and a binary asset. "quokka" appears in one
// document's BODY and in no path or heading, so a search for it can only match
// through the text index. The README carries one of every link kind the shell
// tells apart: markdown in this base, a file it cannot render, another origin,
// a non-web target, and a link the sanitizer strips the target from.
export const knowledgeBaseFiles: Record<string, string> = {
  'README.md':
    '# Knowledge base root\n\nStart with the [setup guide](guides/setup.md) ' +
    'or the [api reference](reference/api.md).\n\n' +
    'Also [the project site](https://example.com/docs), the [raw page](notes/page.html), ' +
    '[write in](mailto:nobody@example.com) and <a href="javascript:void(0)">a stripped link</a>.\n\n' +
    '## Overview\n\n' + kbFiller +
    '## What is inside\n\n' + kbFiller,
  'guides/setup.md':
    '# Setup\n\n[Tuning](advanced/tuning.md) goes deeper.\n\n' +
    '## Install the daemon\n\n' + kbFiller +
    '## Configure the listener\n\nThe quokka setting is documented here.\n\n' + kbFiller +
    '## Verify the install\n\n' + kbFiller,
  'guides/advanced/tuning.md': '# Tuning\n\n[Back to the setup guide](../setup.md).\n\n' + '## Cache sizing\n\n' + kbFiller,
  'reference/api.md': '# API reference\n\n## Endpoints\n\n' + kbFiller,
  'notes/page.html': '<!doctype html><title>raw page</title><p>served raw</p>',
  'assets/logo.png': '\x89PNG\r\n\x1a\n\x00not really a png',
};

// kbPaths is every path in knowledgeBaseFiles, sorted.
export const kbPaths = [
  'README.md', 'assets/logo.png', 'guides/advanced/tuning.md',
  'guides/setup.md', 'notes/page.html', 'reference/api.md',
];

// deepNames are ten nested folder names, long enough that the full path cannot
// fit the breadcrumb bar at any viewport this suite uses.
export const deepNames = [
  'architecture-decisions', 'platform-services', 'identity-and-access',
  'session-management', 'token-lifecycle', 'refresh-strategies',
  'rotation-policies', 'failure-modes', 'observability-hooks', 'runbooks',
];

export const deepDir = deepNames.join('/');
export const deepFile = deepDir + '/level.md';

// deepBaseFiles is a base ten folders deep, one document per level and two at
// the bottom, so navigating it means walking a level at a time and switching
// documents at the same depth.
export const deepBaseFiles: Record<string, string> = (() => {
  const files: Record<string, string> = { 'README.md': '# Deep base\n\nTen folders below this one.\n' };
  deepNames.forEach((_, i) => {
    const dir = deepNames.slice(0, i + 1).join('/');
    files[dir + '/level.md'] = `# Level ${i + 1}\n\nThis document lives at ${dir}.\n`;
  });
  files[deepDir + '/notes.md'] = '# Deep notes\n\nA second document at the bottom.\n';
  return files;
})();

// wideContentFiles is what a reading pane has to CONTAIN rather than be widened
// by: a twelve-column table and a three-hundred-character code line.
export const wideContentFiles: Record<string, string> = (() => {
  const head = Array.from({ length: 12 }, (_, i) => `column ${i + 1} header`);
  const rule = Array.from({ length: 12 }, () => '---');
  const cells = Array.from({ length: 12 }, (_, i) => `row value ${i + 1}`);
  const table =
    '| ' + head.join(' | ') + ' |\n' +
    '| ' + rule.join(' | ') + ' |\n' +
    '| ' + cells.join(' | ') + ' |\n';
  return { 'README.md': '# Wide content\n\n' + table + '\n```\n' + 'x'.repeat(300) + '\n```\n' };
})();

export type KBView = Awaited<ReturnType<typeof readKBView>>;

// readKBView reads the shell's whole visible state in one pass, so a failure
// names every signal rather than only the first.
export function readKBView(page: Page) {
  return page.evaluate(() => {
    const doc = document.getElementById('kb-doc')!;
    const tree = document.getElementById('kb-tree')!;
    const crumbs = document.getElementById('kb-crumbs')!;
    const here = tree.querySelector<HTMLElement>('.kb-tree-here');
    const up = tree.querySelector<HTMLElement>('button.kb-up');
    const more = crumbs.querySelector('.kb-crumb-more');
    const text = (el: Element | null) => (el ? el.textContent!.trim() : '');
    const all = <T extends Element = HTMLElement>(sel: string) => Array.from(document.querySelectorAll<T>(sel));
    return {
      path: location.pathname + location.hash,
      docPath: doc.dataset.path || '',
      busy: doc.getAttribute('aria-busy') || '',
      heading: text(doc.querySelector('h1')),
      crumbs: all('#kb-crumbs .kb-crumb').filter((c) => !c.hidden).map((c) => c.dataset.crumbKind + ':' + text(c)),
      crumbsFit: crumbs.scrollWidth <= crumbs.clientWidth,
      ellipsis: !!more,
      expanded: more ? more.getAttribute('aria-expanded') : '',
      treeReady: tree.dataset.treeReady || '',
      treeFiles: tree.dataset.files || '',
      here: here ? here.dataset.dir! : '',
      hasParent: !!up,
      parent: up ? up.dataset.parent! : '',
      filePaths: all('#kb-tree a.kb-file').map((a) => a.dataset.path!).sort(),
      dirPaths: all('#kb-tree button.kb-dir').map((b) => b.dataset.dir!).sort(),
      current: all('#kb-tree a.kb-file.current').map((a) => a.dataset.path + ':' + a.getAttribute('aria-current')),
      tocHidden: document.getElementById('kb-toc')!.hidden,
      tocLinks: all('#kb-toc-list a.kb-toc-link').map((a) => a.dataset.targetId!),
      tocActive: all("#kb-toc-list a.kb-toc-link[aria-current='true']").map((a) => a.dataset.targetId!),
      marker: ((window as unknown as { __kbMarker?: string }).__kbMarker || '') as string,
      historyLength: history.length,
      pageFits: document.documentElement.scrollWidth <= window.innerWidth,
      // One line per link in the document: where it points, whether the shell
      // marked it as leaving the base, and how it opens.
      docLinks: all<HTMLAnchorElement>('#kb-doc a').map((a) =>
        [
          a.getAttribute('href') || '',
          a.dataset.external || '',
          a.target || '',
          a.rel || '',
          a.querySelector('.kb-ico-ext') ? 'mark' : '',
        ].join('|'),
      ),
    };
  });
}

// readKBTarget reads where the fragment's heading sits relative to the reading pane.
export function readKBTarget(page: Page) {
  return page.evaluate(() => {
    const main = document.getElementById('kb-main')!;
    const id = decodeURIComponent(location.hash.replace(/^#/, ''));
    const el = id ? document.getElementById(id) : null;
    const mr = main.getBoundingClientRect();
    const er = el ? el.getBoundingClientRect() : null;
    return {
      hash: location.hash,
      id,
      found: !!el,
      inView: !!er && er.top >= mr.top && er.bottom <= mr.bottom,
    };
  });
}

// readKBSearch reads the search surface: the results, and what the index says
// it covers.
export function readKBSearch(page: Page) {
  return page.evaluate(() => {
    const note = document.getElementById('kb-index-note')!;
    return {
      state: document.body.dataset.kbIndex || '',
      hidden: document.getElementById('kb-results')!.hidden,
      noteHidden: note.hidden,
      noteText: note.textContent!.trim(),
      indexed: note.dataset.indexed || '',
      total: note.dataset.total || '',
      emptyHidden: document.getElementById('kb-results-empty')!.hidden,
      truncated: !!document.getElementById('kb-results-truncated'),
      hits: Array.from(document.querySelectorAll<HTMLElement>('#kb-hits .kb-hit')).map((a) => ({
        path: a.dataset.path!,
        kind: a.dataset.kind!,
        hash: a.dataset.hash || '',
        external: a.dataset.external || '',
        text: a.textContent!.trim(),
      })),
    };
  });
}

// readKBPanes reads the two sidebars, which are columns on a wide screen and
// drawers on a narrow one. Positions rather than transforms: a drawer is open
// when it is on screen, which is true whatever animates it there.
export function readKBPanes(page: Page) {
  return page.evaluate(() => {
    const rect = (id: string) => document.getElementById(id)!.getBoundingClientRect();
    const tree = rect('kb-tree'), toc = rect('kb-toc');
    return {
      bodyClasses: document.body.className,
      scrimDisplay: getComputedStyle(document.getElementById('kb-scrim')!).display,
      treeLeft: tree.left,
      treeRight: tree.right,
      tocLeft: toc.left,
      tocRight: toc.right,
      width: window.innerWidth,
      pageWidth: document.documentElement.scrollWidth,
    };
  });
}

// readKBLinkColors compares a link the sanitizer disarmed against a live one
// and against the document's own text, which is what "renders as plain text"
// means.
export function readKBLinkColors(page: Page) {
  return page.evaluate(() => {
    const doc = document.getElementById('kb-doc')!;
    const links = Array.from(doc.querySelectorAll('a'));
    const dead = links.find((a) => !a.hasAttribute('href'));
    const live = links.find((a) => a.getAttribute('href') === 'guides/setup.md');
    return {
      doc: getComputedStyle(doc).color,
      dead: dead ? getComputedStyle(dead).color : '',
      live: live ? getComputedStyle(live).color : '',
    };
  });
}

// settled is the selector for a document the shell has finished painting:
// openDoc sets the path and marks the article busy, and paint clears busy.
export function settled(path: string) {
  return `#kb-doc[data-path="${path}"]:not([aria-busy])`;
}

// atFolder is the selector for the sidebar showing one folder's level.
export function atFolder(dir: string) {
  return `#kb-tree .kb-tree-here[data-dir="${dir}"]`;
}

// clickSel clicks through the DOM rather than at a coordinate: the shell's own
// handlers key on an unmodified left button, which a synthesized click is, and
// no assertion here is about hit-testing.
export async function clickSel(page: Page, sel: string) {
  await page.evaluate((sel) => {
    const el = document.querySelector<HTMLElement>(sel);
    if (!el) throw new Error('no element for ' + sel);
    el.click();
  }, sel);
}

// drill walks the sidebar down one folder at a time, which is the only way it
// reaches a nested folder: the sidebar shows one level, never a whole tree.
export async function drill(page: Page, dirs: string[]) {
  for (let i = 0; i < dirs.length; i++) {
    const path = dirs.slice(0, i + 1).join('/');
    await clickSel(page, `#kb-tree button.kb-dir[data-dir="${path}"]`);
    await expect(page.locator(atFolder(path))).toBeVisible();
  }
}

// clearSearch empties the search box. Assigned directly because the shell
// reads the field rather than the keystrokes.
export async function clearSearch(page: Page) {
  await page.evaluate(() => {
    (document.getElementById('kb-search') as HTMLInputElement).value = '';
  });
}

// get fetches a URL the page linked to, for an assertion about what the server
// serves rather than about what the shell drew.
export async function get(url: string) {
  const r = await fetch(url);
  const buf = Buffer.from(await r.arrayBuffer()).subarray(0, 4096);
  return { body: buf.toString(), contentType: r.headers.get('content-type') || '' };
}

// headingReached is true once the shell has scrolled a heading to or below the
// reading pane's top.
export function headingReached(page: Page, id: string) {
  return page.evaluate((id) => {
    const main = document.getElementById('kb-main')!;
    const el = document.getElementById(id);
    return !!el && el.getBoundingClientRect().top >= main.getBoundingClientRect().top;
  }, id);
}

export function treeLeft(page: Page) {
  return page.evaluate(() => document.getElementById('kb-tree')!.getBoundingClientRect().left);
}

export function treeRight(page: Page) {
  return page.evaluate(() => document.getElementById('kb-tree')!.getBoundingClientRect().right);
}
