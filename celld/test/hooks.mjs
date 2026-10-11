// Resolves what the Workers runtime provides to the Worker source and bundle
// but Node does not: the shells module, which the build's esbuild plugin
// supplies, and a .wasm import, which the runtime hands over compiled.
import { registerHooks } from "node:module";

import { SHELLS_MODULE, shellsModule } from "../build/shells.mjs";

registerHooks({
  resolve(specifier, context, next) {
    return specifier === SHELLS_MODULE ? { url: SHELLS_MODULE, shortCircuit: true } : next(specifier, context);
  },
  load(url, context, next) {
    if (url === SHELLS_MODULE) {
      return { format: "module", source: shellsModule(), shortCircuit: true };
    }
    if (url.startsWith("file:") && url.endsWith(".wasm")) {
      const source = `import { readFileSync } from "node:fs";\n`
        + `export default new WebAssembly.Module(readFileSync(new URL(${JSON.stringify(url)})));\n`;
      return { format: "module", source, shortCircuit: true };
    }
    return next(url, context);
  },
});
