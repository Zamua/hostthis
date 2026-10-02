import { test, expect } from '../fixtures';

// Tilde fences avoid escaping backticks inside a template literal.
const markdownFixture = `# Renderer proof

A paragraph with **bold text** and *italic text*.

> Rendered in the browser, never on the server.

| kind     | renderer   |
| -------- | ---------- |
| markdown | marked     |
| mermaid  | mermaid.js |
| csv      | duckdb     |

~~~go
func main() {
	fmt.Println("fenced")
}
~~~

- [x] upload over ssh
- [ ] render in the browser
`;

// The markdown shell turns an uploaded document into a rendered DOM: heading,
// inline emphasis, table, blockquote, language-tagged fence and task list.
test('markdown render', async ({ server, page, pageErrors, shot }) => {
  const paste = server.upload(markdownFixture, { type: 'md', name: 'markdown render' });
  await page.goto(paste.url);

  // The document arrives in one innerHTML assignment and heading ids are added
  // after it, so an anchored h1 means every other block is already in the DOM.
  await expect(page.locator('#content h1.anchored')).toBeVisible();
  const got = await page.evaluate(() => {
    const root = document.getElementById('content')!;
    const text = (el: Element | null) => (el ? el.textContent!.trim() : '');
    const all = (sel: string) => Array.from(root.querySelectorAll(sel));
    const h1 = root.querySelector('h1');
    const code = root.querySelector('pre > code');
    const boxes = all('li input[type="checkbox"]') as HTMLInputElement[];
    return {
      heading: text(h1),
      headingID: h1 ? h1.id : '',
      strong: all('strong').map(text),
      em: all('em').map(text),
      blockquote: text(root.querySelector('blockquote')),
      tableHead: all('table thead th').map(text),
      tableRows: all('table tbody tr').map((tr) => Array.from((tr as HTMLTableRowElement).cells).map(text)),
      codeClass: code ? code.className : '',
      codeText: code ? code.textContent! : '',
      tasks: boxes.length,
      tasksDone: boxes.filter((b) => b.checked).length,
    };
  });
  await shot('rendered');

  expect.soft(got.heading).toBe('Renderer proof');
  expect.soft(got.headingID).toBe('renderer-proof');
  expect.soft(got.strong).toEqual(['bold text']);
  expect.soft(got.em).toEqual(['italic text']);
  expect.soft(got.blockquote).toBe('Rendered in the browser, never on the server.');
  expect.soft(got.tableHead).toEqual(['kind', 'renderer']);
  expect.soft(got.tableRows).toEqual([
    ['markdown', 'marked'],
    ['mermaid', 'mermaid.js'],
    ['csv', 'duckdb'],
  ]);
  expect.soft(got.codeClass).toBe('language-go');
  expect.soft(got.codeText).toContain('fmt.Println("fenced")');
  expect.soft([got.tasks, got.tasksDone]).toEqual([2, 1]);
  pageErrors.expectNone();
});
