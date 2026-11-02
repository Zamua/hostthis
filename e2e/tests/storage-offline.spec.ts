import { test, expect } from '../fixtures';

const OFFLINE = "this paste's storage is offline right now";

// Uploads each shell can render; the stored bytes never reach the page here.
const kinds: Array<[string, string]> = [
  ['md', '# a document\n'],
  ['mermaid', 'flowchart TD\n  A --> B\n'],
  ['csv', 'a,b\n1,2\n'],
  ['diff', '--- a/x\n+++ b/x\n@@ -1 +1 @@\n-a\n+b\n'],
  ['log', '{"level":"info","msg":"hi"}\n'],
  ['flamegraph', 'main;work 10\n'],
  ['text', 'plain words\n'],
];

// A 503 read (an owner's pod storage offline) shows the server's explanation
// in place of the document, in every shell that fetches the raw bytes.
for (const [type, content] of kinds) {
  test(`storage offline: ${type}`, async ({ server, page, shot }) => {
    const paste = server.upload(content, { type, name: `offline ${type}` });
    await page.route(/[?&]raw=1/, (route) =>
      route.fulfill({ status: 503, contentType: 'text/plain; charset=utf-8', body: OFFLINE + '\n' }));
    await page.goto(paste.url);
    await expect(page.getByText(OFFLINE)).toBeVisible();
    await shot(`offline ${type}`);
  });
}
