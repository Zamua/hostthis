import { test, expect } from '../fixtures';

// Exactly four nodes and three edges, with labels distinctive enough that
// finding them cannot be a coincidental match on the shell's own chrome.
const mermaidSource = `flowchart TD
    A[Upload over ssh] --> B{Sniff the kind}
    B -->|mermaid| C[Mermaid shell]
    B -->|markdown| D[Markdown shell]
`;

const wantNodeLabels = ['Upload over ssh', 'Sniff the kind', 'Mermaid shell', 'Markdown shell'];

// The sharpest case of the blank-page class: the diagram is rendered
// client-side, so a bundle that fails to load leaves the paste unrendered while
// every status code stays 200. An svg carrying this source's nodes and edges
// exists only if the renderer actually ran.
test('mermaid render', async ({ server, page, pageErrors, shot }) => {
  const paste = server.upload(mermaidSource, { type: 'mermaid', name: 'mermaid proof' });
  await page.goto(paste.url);

  // Both outcomes end the shell's work, so waiting on the pair reports a
  // failed render as the message the shell wrote instead of as a timeout.
  await expect(page.locator('#content svg, #content .err').first()).toBeVisible();
  await shot('rendered');

  const shellErr = await page.evaluate(() => document.querySelector('#content .err')?.textContent || '');
  // Labels are read off the node label elements rather than the svg's
  // textContent, which also carries the edge labels.
  const d = await page.evaluate(() => {
    const svg = document.querySelector('#content svg');
    if (!svg) return null;
    const box = svg.getBoundingClientRect();
    return {
      nodes: svg.querySelectorAll('.pele-node').length,
      edges: svg.querySelectorAll('.pele-edge').length,
      nodeLabels: [...svg.querySelectorAll('.pele-node .pele-label')].map((e) => e.textContent!.trim()),
      width: box.width,
      height: box.height,
    };
  });

  expect(shellErr, 'shell rendered its failure message').toBe('');
  if (!d) throw new Error('no svg under #content: the mermaid renderer did not run');
  expect.soft([d.nodes, d.edges], 'svg nodes and edges: the element is empty or partly drawn')
    .toEqual([wantNodeLabels.length, 3]);
  expect.soft(d.nodeLabels).toEqual(wantNodeLabels);
  expect.soft(d.width > 0 && d.height > 0, `svg lays out to ${d.width}x${d.height}: nothing is on screen`)
    .toBe(true);

  pageErrors.expectNone();
});

// A mermaid fence in a markdown paste lazy-loads the same renderer; a broken
// fence keeps its source on screen instead of leaving a hole in the document.
test('mermaid fences in markdown', async ({ server, page, pageErrors, shot }) => {
  const md = '# Diagrams\n\n```mermaid\n' + mermaidSource + '```\n\n```mermaid\nflowchart TD\n  A[unclosed --> \n```\n';
  const paste = server.upload(md, { type: 'md', name: 'mermaid fences' });
  await page.goto(paste.url);

  await expect(page.locator('.mermaid-block svg')).toBeVisible();
  await expect(page.locator('.mermaid-block.mermaid-err')).toBeVisible();
  await shot('rendered');

  const got = await page.evaluate(() => ({
    labels: [...document.querySelectorAll('.mermaid-block svg .pele-node .pele-label')].map((e) => e.textContent!.trim()),
    broken: document.querySelector('.mermaid-block.mermaid-err')!.textContent!,
  }));
  expect.soft(got.labels).toEqual(wantNodeLabels);
  expect.soft(got.broken).toContain('A[unclosed -->');
  pageErrors.expectNone();
});

// A diagram the renderer rejects ends in the shell's own message, which quotes
// the parser's error rather than leaving the page on "rendering diagram".
test('mermaid syntax error', async ({ server, page, pageErrors }) => {
  const paste = server.upload('flowchart TD\n  A[unclosed --> \n', { type: 'mermaid', name: 'mermaid broken' });
  await page.goto(paste.url);

  const err = page.locator('#content .err');
  await expect(err).toBeVisible();
  await expect(err).toContainText('Diagram failed to render.');
  await expect(err).toContainText('Parse error');
  await expect(page.locator('#content svg')).toHaveCount(0);
  pageErrors.expectNone();
});
