// Shared test doubles for the cell classes: an in-memory storage that can
// crash after any durable commit, and the seed builders both suites use.

export class SimulatedCrash extends Error {}

export const PASTE_CELL_ID = "a".repeat(64);
export const ROOM_CELL_ID = "b".repeat(64);
export const ROOM_ID = "11111111-1111-4111-8111-111111111111";

export function fakeCellID(value) {
  return { toString() { return value; } };
}

export function clone(value) {
  return value === undefined ? undefined : structuredClone(value);
}

// Every put, delete, alarm change, and transaction is one durable commit.
// Setting crashAfter = n throws SimulatedCrash at the n-th commit so a test
// can inspect the state left behind. A staged instance is a transaction's
// scratch copy and never counts commits.
export class FakeStorage {
  constructor(seed = new Map(), staged = false, alarm = null) {
    this.data = new Map([...seed].map(([key, value]) => [key, clone(value)]));
    this.staged = staged;
    this.alarm = alarm;
    this.commits = 0;
    this.crashAfter = Infinity;
  }

  async get(key) {
    return clone(this.data.get(key));
  }

  async put(keyOrEntries, value) {
    const entries = typeof keyOrEntries === "string"
      ? [[keyOrEntries, value]]
      : keyOrEntries instanceof Map
        ? [...keyOrEntries]
        : Object.entries(keyOrEntries);
    for (const [key, entry] of entries) {
      this.data.set(String(key), clone(entry));
    }
    this.afterCommit();
  }

  async delete(keyOrKeys) {
    const keys = Array.isArray(keyOrKeys) ? keyOrKeys : [keyOrKeys];
    for (const key of keys) {
      this.data.delete(String(key));
    }
    this.afterCommit();
  }

  async list({ prefix = "" } = {}) {
    return new Map([...this.data]
      .filter(([key]) => key.startsWith(prefix))
      .map(([key, value]) => [key, clone(value)]));
  }

  async getAlarm() {
    return this.alarm;
  }

  async setAlarm(at) {
    this.alarm = at;
    this.afterCommit();
  }

  async deleteAlarm() {
    this.alarm = null;
    this.afterCommit();
  }

  async transaction(callback) {
    const tx = new FakeStorage(this.data, true, this.alarm);
    const result = await callback(tx);
    this.data = tx.data;
    this.alarm = tx.alarm;
    this.afterCommit();
    return result;
  }

  afterCommit() {
    if (this.staged) {
      return;
    }
    this.commits++;
    if (this.commits === this.crashAfter) {
      throw new SimulatedCrash("process stopped after a durable commit");
    }
  }
}

export function state(storage) {
  return {
    storage,
    acceptWebSocket() {},
    getWebSockets() { return []; },
    blockConcurrencyWhile(callback) { return callback(); },
  };
}

// One budgeted value of `bytes` under "key", or the given kv with nothing
// budgeted at all.
export function roomDoc({ bytes = 4, kv } = {}) {
  const meta = { appSlug: "appslug1", id: ROOM_ID, createdAt: 1, updatedAt: 1 };
  if (kv) {
    return { meta, kv, wire: {}, bytes: 0, seq: 0, budgetVersion: 0 };
  }
  const text = "a".repeat(bytes);
  return {
    meta,
    kv: { key: Buffer.from(text).toString("base64") },
    wire: { key: JSON.stringify(text) },
    bytes,
    seq: 7,
    budgetVersion: 0,
  };
}

export function putBody(text, appCap = 10) {
  return {
    key: "key",
    value: Buffer.from(text).toString("base64"),
    wire: JSON.stringify(text),
    roomCap: 100,
    keyCap: 10,
    appCap,
    now: 9,
  };
}
