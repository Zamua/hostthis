import { execFileSync } from 'node:child_process';
import { mkdtempSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join, resolve } from 'node:path';

// Builds the daemon once and mints the one SSH identity every upload uses: one
// key for the suite keeps the per-subnet fresh-key gate from spending a slot
// per paste. Workers inherit the paths through the environment.
export default function globalSetup() {
  const dir = mkdtempSync(join(tmpdir(), 'hostthis-e2e-'));
  const root = resolve(import.meta.dirname, '..');
  const bin = join(dir, 'hostthisd');
  // GOWORK=off links the go.mod pins: a local go.work would let the suite pass
  // against a binary CI can never produce.
  execFileSync('go', ['build', '-o', bin, './cmd/hostthisd'], {
    cwd: root,
    env: { ...process.env, GOWORK: 'off' },
    stdio: 'inherit',
  });
  const key = join(dir, 'id_ed25519');
  execFileSync('ssh-keygen', ['-q', '-t', 'ed25519', '-N', '', '-f', key]);
  process.env.HOSTTHIS_E2E_BIN = bin;
  process.env.HOSTTHIS_E2E_KEY = key;
  process.env.HOSTTHIS_E2E_ROOT = root;
  return () => rmSync(dir, { recursive: true, force: true });
}
