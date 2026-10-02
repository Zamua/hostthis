import { test, expect } from '../fixtures';

// Reuses the csv fixture's trick: the scores order differently under numeric
// and lexicographic comparison, so a console that read score as text would
// return a different permutation from ORDER BY, not the same one.
const jsonFixture = `[
  {"name": "delta", "score": 42, "region": "west"},
  {"name": "alpha", "score": 7, "region": "east"},
  {"name": "charlie", "score": 113, "region": "north"},
  {"name": "bravo", "score": 29, "region": "south"},
  {"name": "echo", "score": 88, "region": "east"}
]`;

// Typed at the editor's initial cursor (position 0), so the trailing comment
// marker disarms the default query it lands in front of.
const sortQuery = 'SELECT name, score FROM data ORDER BY score; --';

// The json viewer renders a collapsible tree with type-classed leaves and a
// live filter, and its SQL console loads the paste through read_json_auto,
// where a numeric ORDER BY proves score was typed as a number rather than text.
test('json render', async ({ server, page, pageErrors, shot }) => {
  const paste = server.upload(jsonFixture, { type: 'json', name: 'json proof' });
  await page.goto(paste.url);

  // Both outcomes end the shell's work: renderTree is the only producer of
  // .tree and the boot failure path is the only producer of .err, while the
  // unrendered page holds a lone div.status.
  await expect(page.locator('#content .tree, #content .err').first()).toBeVisible();
  const shellErr = await page.evaluate(() => document.querySelector('#content .err')?.textContent || '');
  // Direct children at each level, so a nested value cannot stand in for a
  // top-level one.
  const tree = await page.evaluate(() => {
    const root = document.querySelector('#content .tree > details');
    if (!root) return null;
    const items = Array.from(root.querySelectorAll(':scope > details'));
    const text = (el: Element | null) => el!.textContent;
    return {
      root: text(root.querySelector(':scope > summary span')),
      items: items.map((d) => text(d.querySelector(':scope > summary span'))),
      keys: items.map((d) => Array.from(d.querySelectorAll(':scope > div span.k')).map(text)),
      numbers: Array.from(root.querySelectorAll('span.num')).map(text),
      strings: Array.from(root.querySelectorAll('span.s')).map(text),
      summary: document.getElementById('summary')!.textContent,
    };
  });
  await shot('tree');

  expect(shellErr, 'shell rendered its failure message').toBe('');
  if (!tree) throw new Error('no root details under #content .tree: the json renderer did not run');
  expect.soft(tree.summary).toBe('5 items');
  expect.soft(tree.root).toBe('[5]');
  expect.soft(tree.items).toEqual(['{3}', '{3}', '{3}', '{3}', '{3}']);
  const keyRow = ['"name": ', '"score": ', '"region": '];
  expect.soft(tree.keys).toEqual([keyRow, keyRow, keyRow, keyRow, keyRow]);
  // Leaves are classed by the value's runtime type, so five unquoted span.num
  // scores exist only if the document's numbers survived parsing as numbers; a
  // string score would render quoted under span.s instead.
  expect.soft(tree.numbers).toEqual(['42', '7', '113', '29', '88']);
  expect.soft(tree.strings).toEqual([
    '"delta"', '"west"', '"alpha"', '"east"', '"charlie"',
    '"north"', '"bravo"', '"south"', '"echo"', '"east"',
  ]);

  // The filter rebuilds the tree per keystroke, marks the match, and reports
  // the hit count, so a viewer that painted once and died fails here.
  await page.locator('#filter').pressSequentially('charlie');
  // The viewer's own report that the filter matched exactly the one row
  // holding charlie's name.
  await page.waitForFunction(() => document.getElementById('summary')!.textContent === '5 items · 1 matches');
  const marks = await page.evaluate(() => Array.from(document.querySelectorAll('#content mark'), (e) => e.textContent));
  await shot('filter-charlie');
  expect.soft(marks).toEqual(['charlie']);

  // The console's editor mounts asynchronously after the click.
  await page.locator('#sqlbtn').click();
  const editor = page.locator('#editor .cm-content');
  await expect(editor).toBeVisible();
  await editor.pressSequentially(sortQuery);
  // Escape closes the completion popup so it cannot sit over the Run button
  // when the click lands.
  await page.keyboard.press('Escape');

  // The keystrokes went through CodeMirror, so what the editor holds is what
  // Run will execute; checking it first makes a mistyped query fail as one.
  const typed = (await editor.textContent()) ?? '';
  if (!typed.startsWith(sortQuery)) {
    await shot('stuck-typing');
    throw new Error(`editor holds ${JSON.stringify(typed)}, want prefix ${JSON.stringify(sortQuery)}`);
  }
  await shot('sql-console');

  // The first run also fetches and instantiates duckdb, all served locally.
  await page.locator('#run').click();
  await expect(page.locator('#content table, #content .err').first()).toBeVisible();
  // Unambiguous because the json data view is a tree: any table under #content
  // is the console's.
  const res = await page.evaluate(() => {
    const err = document.querySelector('#content .err');
    if (err) return { err: err.textContent };
    const rows = Array.from(document.querySelectorAll<HTMLTableRowElement>('#content table tbody tr'));
    return {
      err: '',
      cols: Array.from(document.querySelectorAll('#content table thead th'), (e) => e.textContent),
      names: rows.map((r) => r.cells[0].textContent),
      scores: rows.map((r) => r.cells[1].textContent),
      scoreClasses: rows.map((r) => r.cells[1].className),
      tab: document.querySelector('#views [data-view="result"]')!.textContent,
    };
  });
  await shot('result-sorted');

  expect(res.err, 'query failed').toBe('');
  expect.soft(res.cols).toEqual(['name', 'score']);
  // Ascending by value: 7, 29, 42, 88, 113. A text-typed score would order
  // lexicographically as charlie, bravo, delta, alpha, echo, and neither
  // matches the source order, so an unsorted table cannot pass either.
  expect.soft(res.names).toEqual(['alpha', 'bravo', 'delta', 'echo', 'charlie']);
  expect.soft(res.scores).toEqual(['7', '29', '42', '88', '113']);
  // A cell is classed "n" only for a number or bigint value, so the engine's
  // own schema typed the column numeric.
  expect.soft(res.scoreClasses).toEqual(['n', 'n', 'n', 'n', 'n']);
  expect.soft(res.tab).toBe('Result · 5');

  pageErrors.expectNone();
});
