import { test, expect } from '../fixtures';

// A two-file git diff so the file-name assertion cannot pass on a single
// accidental match. The counts are asymmetric (4 added, 2 removed, split 3+1
// and 2+0 across the files) so a counter reading the wrong class or the wrong
// file reads as a mismatch, not a coincidence.
const diffFixture = `diff --git a/greet.go b/greet.go
index 1111111..2222222 100644
--- a/greet.go
+++ b/greet.go
@@ -1,7 +1,8 @@
 package main
 
-func greet(name string) string {
-	return "hi " + name
+// greet returns a friendly greeting.
+func greet(name string) string {
+	return "hello, " + name + "!"
 }
 
 func main() {
diff --git a/greet_test.go b/greet_test.go
index 3333333..4444444 100644
--- a/greet_test.go
+++ b/greet_test.go
@@ -3,6 +3,7 @@ package main
 import "testing"
 
 func TestGreet(t *testing.T) {
	t.Parallel()
+	t.Parallel()
 	if greet("x") == "" {
 		t.Fatal("empty greeting")
 	}
`;

// Panes only a side-by-side render emits: two per file. Line-by-line output has
// none, so the count IS the layout mode.
const sidePanes = '#diff .d2h-file-side-diff';

// The diff viewer renders one block per file with diff2html, highlights the
// code, and rebuilds the layout when the side-by-side toggle is pressed. A
// bare git-diff body reaches the same shell with no --type hint.
test('diff render', async ({ server, page, pageErrors, shot }) => {
  const paste = server.upload(diffFixture, { type: 'diff', name: 'diff proof' });

  // Sniffing check: the hunk header alone must route an untyped upload to the
  // diff shell, and diff.js is served by no other shell.
  const sniffed = server.upload(diffFixture);
  await server.waitReady(sniffed);
  const body = await (await fetch(sniffed.url)).text();
  expect.soft(body, `untyped git-diff upload was not served the diff shell; page:\n${body.slice(0, 400)}`)
    .toContain('/_hostthis/diff.js');

  await page.goto(paste.url);

  // aria-busy starts true in the static shell and flips to false only when
  // the renderer finished or the fetch failed, so waiting on it reports a
  // failed load through the assertions below instead of as a timeout.
  await expect(page.locator('#diff[aria-busy="false"]')).toBeVisible();
  await shot('line-by-line');

  const got = await page.evaluate((sidePanes) => {
    const q = (sel: string) => document.querySelectorAll(sel);
    return {
      // Scoped to .d2h-file-wrapper so the file LIST at the top, which repeats
      // the names, is not double-counted.
      fileNames: Array.from(q('#diff .d2h-file-wrapper .d2h-file-name'), (e) => e.textContent!.trim()),
      // The content cell and its line-number cell both carry the change class,
      // so line counts exclude the number cell to count each line once.
      insCount: q('#diff td.d2h-ins:not(.d2h-code-linenumber)').length,
      delCount: q('#diff td.d2h-del:not(.d2h-code-linenumber)').length,
      // Any highlight.js span is proof the highlighter ran over the diffed
      // source; the exact token classes are hljs's.
      hljsCount: q('#diff .d2h-code-line-ctn [class*="hljs-"]').length,
      sideCount: q(sidePanes).length,
    };
  }, sidePanes);

  const diffText = async () => ((await page.locator('#diff').textContent()) ?? '').slice(0, 200);
  expect(got.fileNames, `file blocks\n#diff: ${await diffText()}`).toEqual(['greet.go', 'greet_test.go']);
  expect.soft([got.insCount, got.delCount], 'added and removed lines').toEqual([4, 2]);
  expect.soft(got.hljsCount, 'no highlight.js spans in the code lines: the highlighter did not run')
    .toBeGreaterThan(0);
  // A fresh browser profile has no persisted format, so the first render must
  // be line-by-line, which makes the toggle below an observable change.
  expect(got.sideCount, 'side-by-side panes in the initial render').toBe(0);

  // The toggle re-renders through the same pipeline; waiting on the panes it
  // alone produces means a click that never reached the handler fails as a
  // timeout here rather than as a count mismatch.
  await page.locator('#btn-side').click();
  await expect.poll(() => page.locator(sidePanes).count()).toBeGreaterThan(0);
  const after = await page.evaluate((sidePanes) => ({
    sideCount: document.querySelectorAll(sidePanes).length,
    sidePressed: document.getElementById('btn-side')!.getAttribute('aria-pressed') === 'true',
  }), sidePanes);
  await shot('side-by-side');

  expect.soft(after.sideCount, 'side-by-side panes (two per file)').toBe(4);
  expect.soft(after.sidePressed, 'side-by-side button is aria-pressed after the switch').toBe(true);

  pageErrors.expectNone();
});
