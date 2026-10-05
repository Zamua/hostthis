// Local entry for the celld Worker under workerd. The Worker source runs
// unchanged; this module only adds isolation: a request under /ns/<id>/
// addresses cells named "\0ns:<id>\0<name>", and every cell-to-cell call from
// such a cell stays in the same namespace, so one runtime serves many tests
// without shared state.
import worker, * as cells from "../src/index.js";

// NUL cannot occur in a name the Go adapters build.
const NS_MARK = "\0ns:";
const NAMESPACES = ["IDENTITY", "PASTES", "ROOMS", "SUBNETS"];

function namespacedEnv(env, ns) {
  if (!ns) return env;
  const out = { ...env };
  for (const name of NAMESPACES) {
    out[name] = new Proxy(env[name], {
      get(target, prop) {
        if (prop === "idFromName") return (n) => target.idFromName(NS_MARK + ns + "\0" + n);
        const value = Reflect.get(target, prop, target);
        return typeof value === "function" ? value.bind(target) : value;
      },
    });
  }
  return out;
}

function nsOf(state) {
  const name = state.id.name ?? "";
  if (!name.startsWith(NS_MARK)) return "";
  return name.slice(NS_MARK.length, name.indexOf("\0", NS_MARK.length));
}

function cell(Cls) {
  return class extends Cls {
    constructor(state, env) {
      super(state, namespacedEnv(env, nsOf(state)));
    }
  };
}

export const Identity = cell(cells.Identity);
export const Paste = cell(cells.Paste);
export const Room = cell(cells.Room);
export const Subnet = cell(cells.Subnet);
export const IntentLog = cells.IntentLog;

const NS_PATH = /^\/ns\/([A-Za-z0-9_-]+)(\/.*)$/;

export default {
  async fetch(request, env, ctx) {
    const url = new URL(request.url);
    const m = NS_PATH.exec(url.pathname);
    if (!m) return worker.fetch(request, env, ctx);
    url.pathname = m[2];
    return worker.fetch(new Request(url, request), namespacedEnv(env, m[1]), ctx);
  },
};
