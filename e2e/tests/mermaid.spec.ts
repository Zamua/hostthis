import { test, expect } from '../fixtures';

// Exactly four nodes and three edges, with labels distinctive enough that
// finding them cannot be a coincidental match on the shell's own chrome.
const mermaidSource = `flowchart TD
    A[Upload over ssh] --> B{Sniff the kind}
    B -->|mermaid| C[Mermaid shell]
    B -->|markdown| D[Markdown shell]
`;

const wantNodeLabels = ['Upload over ssh', 'Sniff the kind', 'Mermaid shell', 'Markdown shell'];

// The sharpest case of the blank-page class: mermaid is rendered client-side,
// so a bundle that fails to load leaves the paste unrendered while every status
// code stays 200. An svg carrying this source's nodes and edges exists only if
// mermaid.js actually ran.
test('mermaid render', async ({ server, page, pageErrors, shot }) => {
  const paste = server.upload(mermaidSource, { type: 'mermaid', name: 'mermaid proof' });
  await page.goto(paste.url);

  // Both outcomes end the shell's work, so waiting on the pair reports a
  // failed render as the message the shell wrote instead of as a timeout.
  await expect(page.locator('#content svg, #content .err').first()).toBeVisible();
  await shot('rendered');

  const shellErr = await page.evaluate(() => document.querySelector('#content .err')?.textContent || '');
  // Labels are read off the label elements rather than the svg's textContent,
  // which also carries mermaid's injected stylesheet and would match anything.
  const d = await page.evaluate(() => {
    const svg = document.querySelector('#content svg');
    if (!svg) return null;
    const box = svg.getBoundingClientRect();
    return {
      nodes: svg.querySelectorAll('g.node').length,
      edges: svg.querySelectorAll('g.edgePaths path').length,
      nodeLabels: [...svg.querySelectorAll('g.node .nodeLabel')].map((e) => e.textContent!.trim()),
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
