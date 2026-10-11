import { test, expect } from '../fixtures';

// A script the page asked for and did not get is reported, so a clean error log
// is evidence rather than a filter that swallows everything.
test('harness reports a missing subresource', async ({ server, page, pageErrors }) => {
  const paste = server.upload('# probe\n', { type: 'md' });
  await page.goto(paste.url);
  await expect(page.locator('#content h1')).toBeVisible();
  await page.evaluate(() => {
    document.head.appendChild(Object.assign(document.createElement('script'), { src: '/_hostthis/no-such-asset.js' }));
  });
  // The load failure arrives asynchronously, after the injection returns.
  await expect.poll(() => pageErrors.list().length, { timeout: 10_000 }).toBeGreaterThan(0);
  expect(pageErrors.list().join('\n')).toContain('no-such-asset.js');
});

// Paste URLs name the server E2E_READS picks. Only the Worker refuses a POST
// to a read path, so the answer tells the two apart.
test('harness reads from the server E2E_READS names', async ({ server }) => {
  const paste = server.upload('# which server\n', { type: 'md' });
  expect(new URL(paste.url).origin).toBe(server.baseURL);
  const res = await fetch(paste.url, { method: 'POST' });
  expect(res.status).toBe(server.reads === 'worker' ? 405 : 200);
});
