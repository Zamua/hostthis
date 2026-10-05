import { test as base, expect, type Page, type TestInfo } from '@playwright/test';
import { spawn, spawnSync, type ChildProcess } from 'node:child_process';
import { mkdtempSync, mkdirSync, rmSync, writeFileSync } from 'node:fs';
import { createServer, connect } from 'node:net';
import { tmpdir } from 'node:os';
import { dirname, join } from 'node:path';

export type UploadOpts = { type?: string; name?: string };
export type Paste = { slug: string; url: string };

export class Server {
  readonly baseURL: string;
  private log = '';
  private exited = false;

  constructor(
    private proc: ChildProcess,
    private runtime: Runtime,
    private httpAddr: string,
    readonly sshPort: number,
    private dataDir: string,
  ) {
    this.baseURL = `http://${httpAddr}`;
    proc.stdout?.on('data', (d) => (this.log += d));
    proc.stderr?.on('data', (d) => (this.log += d));
    proc.on('exit', () => (this.exited = true));
  }

  logs() {
    return this.log;
  }

  async waitServing(timeoutMs = 30_000) {
    const deadline = Date.now() + timeoutMs;
    while (Date.now() < deadline) {
      if (this.exited) throw new Error(`hostthisd exited before serving\n${this.log}`);
      if ((await httpUp(`${this.baseURL}/healthz`)) && (await tcpUp(this.sshPort))) return;
      await sleep(50);
    }
    throw new Error(`hostthisd did not serve http+ssh within ${timeoutMs}ms\n${this.log}`);
  }

  async stop() {
    if (!this.exited) {
      this.proc.kill('SIGTERM');
      const killed = await Promise.race([
        new Promise<boolean>((r) => this.proc.once('exit', () => r(false))),
        sleep(10_000).then(() => true),
      ]);
      if (killed) this.proc.kill('SIGKILL');
    }
    await this.runtime.stop();
    rmSync(this.dataDir, { recursive: true, force: true });
  }

  // upload pipes content to the daemon over SSH the way a user does. --name goes
  // last: the server folds every token after it, up to the next flag, into the label.
  upload(content: string | Buffer, opts: UploadOpts = {}): Paste {
    const argv: string[] = [];
    if (opts.type) argv.push('--type', opts.type);
    if (opts.name) argv.push('--name', opts.name);
    const res = spawnSync(
      'ssh',
      [
        '-T', '-i', process.env.HOSTTHIS_E2E_KEY!, '-p', String(this.sshPort),
        '-o', 'IdentitiesOnly=yes', '-o', 'StrictHostKeyChecking=no',
        '-o', 'UserKnownHostsFile=/dev/null', '-o', 'LogLevel=ERROR',
        // '--' ends ssh's own option parsing, so a remote flag like --type
        // reaches the server instead of being read as an ssh option.
        '--', 'e2e@127.0.0.1', argv.join(' '),
      ],
      { input: content, timeout: 30_000 },
    );
    const out = res.stdout.toString().trim();
    if (res.status !== 0) throw new Error(`ssh upload exited ${res.status}: ${res.stderr}`);
    const m = new URL(out).pathname.match(/^\/p\/([^/]+)$/);
    if (!m) throw new Error(`upload printed ${JSON.stringify(out)}, not a path-mode paste url`);
    return { slug: m[1], url: out };
  }

  // uploadDir sends a gzip tar of the files, each entry named by its key.
  uploadDir(files: Record<string, string>, opts: UploadOpts = {}): Paste {
    const dir = mkdtempSync(join(tmpdir(), 'hostthis-e2e-dir-'));
    try {
      const names = Object.keys(files).sort();
      for (const name of names) {
        mkdirSync(dirname(join(dir, name)), { recursive: true });
        writeFileSync(join(dir, name), files[name]);
      }
      // COPYFILE_DISABLE keeps macOS tar from adding ._ AppleDouble entries.
      const tar = spawnSync('tar', ['czf', '-', '-C', dir, ...names], {
        env: { ...process.env, COPYFILE_DISABLE: '1' },
        maxBuffer: 64 << 20,
      });
      if (tar.status !== 0) throw new Error(`tar: ${tar.stderr}`);
      return this.upload(tar.stdout, opts);
    } finally {
      rmSync(dir, { recursive: true, force: true });
    }
  }

  // waitReady blocks until the paste serves its own content rather than the
  // pending page. Header assertions need it; a browser rides the pending page's
  // meta refresh. Retry-After is set by the pending page alone.
  async waitReady(p: Paste, timeoutMs = 20_000) {
    const deadline = Date.now() + timeoutMs;
    while (Date.now() < deadline) {
      try {
        const r = await fetch(p.url, { redirect: 'manual' });
        if (r.status === 410) throw new Error(`paste ${p.slug} failed to store\n${this.log}`);
        if (r.status === 200 && !r.headers.get('retry-after')) return;
      } catch (e) {
        if (e instanceof Error && e.message.includes('failed to store')) throw e;
      }
      await sleep(50);
    }
    throw new Error(`paste ${p.slug} still pending after ${timeoutMs}ms\n${this.log}`);
  }
}

// Runtime is the celld Worker under Miniflare (celld/localrt), one per worker,
// so each daemon owns its cells. It exits when its stdin closes.
class Runtime {
  private constructor(
    private proc: ChildProcess,
    readonly url: string,
  ) {}

  static async start(timeoutMs = 30_000): Promise<Runtime> {
    const proc = spawn('node', ['serve.mjs'], {
      cwd: join(process.env.HOSTTHIS_E2E_ROOT!, 'celld', 'localrt'),
      stdio: ['pipe', 'pipe', 'inherit'],
    });
    let out = '';
    const url = await new Promise<string>((res, rej) => {
      const timer = setTimeout(() => rej(new Error(`celld runtime not ready after ${timeoutMs}ms`)), timeoutMs);
      proc.on('exit', (code) => rej(new Error(`celld runtime exited ${code} before serving`)));
      proc.stdout!.on('data', (d) => {
        out += d;
        const m = out.match(/^LISTENING (\S+)$/m);
        if (m) {
          clearTimeout(timer);
          res(m[1]);
        }
      });
    });
    return new Runtime(proc, url);
  }

  async stop() {
    if (this.proc.exitCode !== null) return;
    const exited = new Promise((r) => this.proc.once('exit', r));
    this.proc.stdin!.end();
    await Promise.race([exited, sleep(10_000).then(() => this.proc.kill('SIGKILL'))]);
  }
}

// PageErrors is the cheap half of the blank-page check: a shell whose bundle
// 404s or throws renders nothing and says so only here.
export class PageErrors {
  private problems: string[] = [];
  private ignored: string[] = [];

  constructor(page: Page) {
    page.on('pageerror', (e) => this.add(`uncaught exception: ${e.message}`));
    page.on('console', (m) => {
      if (m.type() === 'error') this.add(`console.error: ${m.text()} ${m.location().url}`);
    });
    page.on('requestfailed', (r) => {
      if (!isFavicon(r.url())) this.add(`request failed: ${r.failure()?.errorText} ${r.url()}`);
    });
    page.on('response', (r) => {
      if (r.status() >= 400 && !isFavicon(r.url())) this.add(`network error: ${r.status()} ${r.url()}`);
    });
  }

  // ignore drops problems naming a URL a fixture asks for ON PURPOSE. Keep the
  // substring as specific as the URL: a loose one silences real evidence.
  ignore(substr: string) {
    this.ignored.push(substr);
  }

  list() {
    return this.problems.filter((p) => !this.ignored.some((s) => p.includes(s)));
  }

  expectNone() {
    expect(this.list(), 'page reported errors').toEqual([]);
  }

  private add(msg: string) {
    this.problems.push(msg.trim());
  }
}

// No shell references a favicon, so ignoring that one path cannot mask a
// subresource the page asked for.
function isFavicon(u: string) {
  try {
    return new URL(u).pathname === '/favicon.ico';
  } catch {
    return false;
  }
}

type TestFixtures = {
  pageErrors: PageErrors;
  shot: (label: string) => Promise<void>;
  serverLog: void;
};

export const test = base.extend<TestFixtures, { server: Server }>({
  server: [
    async ({}, use) => {
      const runtime = await Runtime.start();
      const ports = await freePorts(3);
      const httpAddr = `127.0.0.1:${ports[0]}`;
      const dataDir = mkdtempSync(join(tmpdir(), 'hostthis-e2e-data-'));
      const env = Object.fromEntries(
        Object.entries(process.env).filter(([k]) => !k.startsWith('HOSTTHIS_') || k.startsWith('HOSTTHIS_E2E_')),
      );
      const proc = spawn(process.env.HOSTTHIS_E2E_BIN!, [], {
        env: {
          ...env,
          HOSTTHIS_URL_MODE: 'path',
          HOSTTHIS_PUBLIC_SCHEME: 'http',
          HOSTTHIS_APEX_DOMAIN: httpAddr,
          HOSTTHIS_HTTP_ADDR: httpAddr,
          HOSTTHIS_SSH_ADDR: `127.0.0.1:${ports[1]}`,
          HOSTTHIS_METRICS_ADDR: `127.0.0.1:${ports[2]}`,
          HOSTTHIS_DATA_DIR: dataDir,
          HOSTTHIS_CELLD_ENDPOINT: runtime.url,
          HOSTTHIS_LANDING: join(process.env.HOSTTHIS_E2E_ROOT!, 'web', 'landing.html'),
        },
      });
      const server = new Server(proc, runtime, httpAddr, ports[1], dataDir);
      await server.waitServing();
      await use(server);
      await server.stop();
    },
    { scope: 'worker' },
  ],

  pageErrors: async ({ page }, use) => {
    await use(new PageErrors(page));
  },

  // shot attaches a full-page screenshot as evidence for a human. Never an
  // assertion: font rasterization differs between machines.
  shot: async ({ page }, use, testInfo) => {
    let step = 0;
    await use(async (label: string) => {
      step++;
      const body = await page.screenshot({ fullPage: true });
      await testInfo.attach(`${String(step).padStart(2, '0')}-${label}`, { body, contentType: 'image/png' });
    });
  },

  serverLog: [
    async ({ server }, use, testInfo: TestInfo) => {
      await use();
      if (testInfo.status !== testInfo.expectedStatus) {
        await testInfo.attach('hostthisd.log', { body: server.logs(), contentType: 'text/plain' });
      }
    },
    { auto: true },
  ],
});

export { expect };

// freePorts holds all n listeners before releasing any, so the kernel cannot
// hand the same port out twice. The gap before the daemon binds is a race no
// API closes short of passing the listeners into the child.
async function freePorts(n: number): Promise<number[]> {
  const servers = await Promise.all(
    Array.from({ length: n }, () => new Promise<ReturnType<typeof createServer>>((res) => {
      const s = createServer();
      s.listen(0, '127.0.0.1', () => res(s));
    })),
  );
  const ports = servers.map((s) => (s.address() as { port: number }).port);
  await Promise.all(servers.map((s) => new Promise((r) => s.close(r))));
  return ports;
}

async function httpUp(url: string) {
  try {
    return (await fetch(url, { signal: AbortSignal.timeout(1000) })).status === 200;
  } catch {
    return false;
  }
}

function tcpUp(port: number) {
  return new Promise<boolean>((res) => {
    const c = connect(port, '127.0.0.1');
    c.once('connect', () => (c.destroy(), res(true)));
    c.once('error', () => res(false));
    c.setTimeout(1000, () => (c.destroy(), res(false)));
  });
}

function sleep(ms: number) {
  return new Promise((r) => setTimeout(r, ms));
}
