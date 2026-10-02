import { test, expect } from '../fixtures';

// Shaped so the sort assertion can fail. Source order is not sorted by any
// column, and the scores order differently under numeric and lexicographic
// comparison, so a viewer that sorted text would produce a different
// permutation rather than the same one.
const csvFixture = `name,score,region
delta,42,west
alpha,7,east
charlie,113,north
bravo,29,south
echo,88,east
`;

// The first header cell is the row-number gutter, which carries no sort, so
// column i is cell i+2.
const scoreHeader = '#content table thead th:nth-child(3)';

// Row ids carry the SOURCE row number, so they stay stable under a sort and
// name the permutation the viewer applied.
const rowOrder = () => Array.from(document.querySelectorAll('#content table tbody tr'), (tr) => tr.id);

// The csv viewer renders a real table and sorts it on a header click. Numeric
// columns sort by value, and the source order is recoverable from row ids.
test('csv render', async ({ server, page, pageErrors, shot }) => {
  const paste = server.upload(csvFixture, { type: 'csv', name: 'csv proof' });
  await page.goto(paste.url);

  // The table is built detached and attached in one statement, so a queryable
  // table is a finished one.
  await expect(page.locator('#content table')).toBeVisible();
  // Column names only: a header cell also carries the sort arrow, the type and
  // the facts line, so its own text is all four joined.
  const cols = await page.evaluate(() =>
    Array.from(document.querySelectorAll('#content table thead th .name'), (e) => e.textContent),
  );
  const sourceOrder = await page.evaluate(rowOrder);
  // textContent, not rendered text: the column type is displayed through
  // text-transform: uppercase, so rendered text would read back as NUMBER.
  const scoreType = await page.evaluate((sel) => document.querySelector(sel)!.textContent, `${scoreHeader} .type`);
  const summary = await page.evaluate(() => document.querySelector('#summary')!.textContent);
  await shot('table');

  expect.soft(cols).toEqual(['name', 'score', 'region']);
  expect(sourceOrder).toEqual(['row-1', 'row-2', 'row-3', 'row-4', 'row-5']);
  // The sort below is numeric only because the column was typed numeric.
  expect.soft(scoreType).toBe('number');
  expect.soft(summary).toBe('5 rows · 3 cols');

  await page.locator(scoreHeader).click();
  // Waiting on the header's own indicator, so a click that never reached the
  // handler fails as a timeout here rather than as an order mismatch.
  await page.waitForFunction((sel) => document.querySelector(`${sel} .arrow`)?.textContent === '▲', scoreHeader);
  const sortedOrder = await page.evaluate(rowOrder);
  await shot('sorted-by-score');

  expect(sortedOrder, 'order unchanged after clicking the score header').not.toEqual(sourceOrder);
  // Ascending by value: 7, 29, 42, 88, 113. Sorting the same column as text
  // would give row-3, row-4, row-1, row-2, row-5.
  expect.soft(sortedOrder).toEqual(['row-2', 'row-4', 'row-1', 'row-5', 'row-3']);

  pageErrors.expectNone();
});
