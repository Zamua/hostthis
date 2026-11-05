import type { Browser, BrowserContext, Page } from '@playwright/test';
import { test, expect, type Server } from '../fixtures';

// docs/SPEC.md "WebRTC (peer-to-peer data, audio and video)": an uploaded app
// signals over its room's WebSocket relay and captures on its own origin. Two
// tabs of one app connect to each other the way two visitors would.

// The app under test. A tab that holds no room creates one; every tab joins the
// room's relay, says hello, and the tab already there makes the offer. Frames
// are addressed by a per-tab id, since the relay copies each to every peer.
const APP = `<!doctype html>
<meta charset="utf-8">
<title>call</title>
<video id="remote" autoplay playsinline muted></video>
<pre id="log"></pre>
<script>
const base = location.pathname.match(/^\\/p\\/[^/]+/)?.[0] ?? '';
const me = crypto.randomUUID();
const log = (s) => { document.getElementById('log').textContent += s + '\\n'; };
window.rtc = { received: [], remoteKinds: [], state: 'new' };

async function start({ room, media }) {
  if (!room) {
    const res = await fetch(base + '/api/rooms', { method: 'POST' });
    room = (await res.json()).id;
  }
  rtc.room = room;
  const local = media ? await navigator.mediaDevices.getUserMedia({ audio: true, video: true }) : null;
  const ws = new WebSocket(location.origin.replace(/^http/, 'ws') + base + '/api/rooms/' + room + '/ws');
  let pc = null;
  const send = (to, body) => ws.send(JSON.stringify({ type: 'rtc', from: me, to, ...body }));
  const connect = (peer) => {
    pc = new RTCPeerConnection({ iceServers: [] });
    pc.onicecandidate = (e) => e.candidate && send(peer, { candidate: e.candidate.toJSON() });
    pc.onconnectionstatechange = () => { rtc.state = pc.connectionState; log('state ' + pc.connectionState); };
    pc.ontrack = (e) => {
      rtc.remoteKinds.push(e.track.kind);
      document.getElementById('remote').srcObject = e.streams[0];
    };
    local?.getTracks().forEach((t) => pc.addTrack(t, local));
    return pc;
  };
  const wire = (dc) => {
    rtc.channel = dc;
    dc.onmessage = (e) => { rtc.received.push(e.data); log('got ' + e.data); };
  };
  ws.onmessage = async (e) => {
    const m = JSON.parse(e.data);
    if (m.type === 'snapshot') { send(null, { hello: true }); return; }
    if (m.type !== 'rtc' || m.from === me || (m.to && m.to !== me)) return;
    if (m.hello && !pc) {
      connect(m.from);
      wire(pc.createDataChannel('chat'));
      await pc.setLocalDescription(await pc.createOffer());
      send(m.from, { description: pc.localDescription.toJSON() });
    } else if (m.description) {
      if (!pc) { connect(m.from); pc.ondatachannel = (ev) => wire(ev.channel); }
      await pc.setRemoteDescription(m.description);
      if (m.description.type === 'offer') {
        await pc.setLocalDescription(await pc.createAnswer());
        send(m.from, { description: pc.localDescription.toJSON() });
      }
      rtc.negotiated = pc.signalingState === 'stable';
    } else if (m.candidate && pc) {
      await pc.addIceCandidate(m.candidate);
    }
  };
  await new Promise((ok, fail) => { ws.onopen = ok; ws.onerror = fail; });
  return room;
}
window.start = start;
</script>
`;

async function openTab(context: BrowserContext, url: string, opts: { room?: string; media: boolean }) {
  const page = await context.newPage();
  const errors: string[] = [];
  page.on('pageerror', (e) => errors.push(e.message));
  await page.goto(url);
  const room = await page.evaluate((o) => (window as any).start(o), opts);
  return { page, room: room as string, errors };
}

// call signals an offer and answer between two tabs over the relay, then waits
// for the peers to connect.
async function call(server: Server, context: BrowserContext, media: boolean) {
  const app = server.uploadDir({ 'index.html': APP });
  await server.waitReady(app);
  const a = await openTab(context, app.url, { media });
  const b = await openTab(context, app.url, { room: a.room, media });
  for (const tab of [a, b]) {
    await expect.poll(() => tab.page.evaluate(() => (window as any).rtc.negotiated)).toBe(true);
  }
  if (test.info().project.name === 'webkit') {
    // Playwright's Linux WebKit negotiates but never gathers a candidate, even
    // between two connections in one page, so connectivity is Chromium's to
    // prove; signaling over the relay is proven in both engines above.
    return { a, b, connected: false };
  }
  for (const tab of [a, b]) {
    await expect.poll(() => tab.page.evaluate(() => (window as any).rtc.state), { timeout: 30_000 }).toBe('connected');
  }
  return { a, b, connected: true };
}

const channelOpen = (p: Page) => p.evaluate(() => (window as any).rtc.channel?.readyState);

test('two visitors open a data channel signaled over the room relay', async ({ server, browser }) => {
  const context = await browser.newContext();
  const { a, b, connected } = await call(server, context, false);
  if (connected) {
    for (const tab of [a, b]) await expect.poll(() => channelOpen(tab.page)).toBe('open');
    await a.page.evaluate(() => (window as any).rtc.channel.send('hello from a'));
    await b.page.evaluate(() => (window as any).rtc.channel.send('hello from b'));
    await expect.poll(() => b.page.evaluate(() => (window as any).rtc.received)).toEqual(['hello from a']);
    await expect.poll(() => a.page.evaluate(() => (window as any).rtc.received)).toEqual(['hello from b']);
  }
  expect([...a.errors, ...b.errors]).toEqual([]);
  await context.close();
});

test.describe('capture', () => {
  // Chromium's fake devices, the chromium project's launch flags
  // (playwright.config.ts), stand in for a camera and microphone; WebKit's
  // headless build has none to grant.
  test.skip(({ browserName }) => browserName !== 'chromium', 'fake capture devices are a Chromium flag');

  const newContext = (browser: Browser) => browser.newContext({ permissions: ['camera', 'microphone'] });

  test('two visitors exchange camera and microphone', async ({ server, browser }) => {
    const context = await newContext(browser);
    const { a, b } = await call(server, context, true);
    for (const tab of [a, b]) {
      await expect.poll(() => tab.page.evaluate(() => (window as any).rtc.remoteKinds.sort())).toEqual(['audio', 'video']);
      // A decoded frame, not just a negotiated track.
      await expect
        .poll(() => tab.page.evaluate(() => (document.getElementById('remote') as HTMLVideoElement).videoWidth))
        .toBeGreaterThan(0);
    }
    await b.page.screenshot().then((body) => test.info().attach('remote-video', { body, contentType: 'image/png' }));
    expect([...a.errors, ...b.errors]).toEqual([]);
    await context.close();
  });

  test('an app may capture on its own origin; a render shell may not', async ({ server, browser }) => {
    const context = await newContext(browser);
    const page = await context.newPage();
    const allows = () =>
      page.evaluate(() =>
        ['camera', 'microphone', 'display-capture'].map((f) => (document as any).featurePolicy.allowsFeature(f)),
      );

    const app = server.uploadDir({ 'index.html': APP });
    await server.waitReady(app);
    await page.goto(app.url);
    expect(await allows()).toEqual([true, true, true]);
    const tracks = await page.evaluate(async () =>
      (await navigator.mediaDevices.getUserMedia({ audio: true, video: true })).getTracks().map((t) => t.kind).sort(),
    );
    expect(tracks).toEqual(['audio', 'video']);

    const doc = server.upload('# notes\n', { type: 'md' });
    await page.goto(doc.url);
    await expect(page.locator('#content h1')).toBeVisible();
    expect((await allows()).slice(0, 2)).toEqual([false, false]);
    const refused = await page.evaluate(() =>
      navigator.mediaDevices.getUserMedia({ video: true }).then(() => 'granted', (e) => e.name),
    );
    expect(refused).toBe('NotAllowedError');
    await context.close();
  });
});
