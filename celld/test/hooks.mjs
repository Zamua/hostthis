// Resolves the shells module for tests that import the Worker source, as the
// build's esbuild plugin does for the bundle.
import { registerHooks } from "node:module";

import { SHELLS_MODULE, shellsModule } from "../build/shells.mjs";

registerHooks({
  resolve(specifier, context, next) {
    return specifier === SHELLS_MODULE ? { url: SHELLS_MODULE, shortCircuit: true } : next(specifier, context);
  },
  load(url, context, next) {
    return url === SHELLS_MODULE
      ? { format: "module", source: shellsModule(), shortCircuit: true }
      : next(url, context);
  },
});
