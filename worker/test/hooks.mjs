// Resolves what the Workers runtime provides to the Worker source and bundle
// but Node does not: the shells and build id modules, which the build's
// esbuild plugins supply, and a .wasm import, which the runtime hands over
// compiled.
import { registerHooks } from "node:module";

import { BUILD_MODULE, buildModule } from "../build/buildid.mjs";
import { SHELLS_MODULE, shellsModule } from "../build/shells.mjs";

const generated = { [SHELLS_MODULE]: shellsModule, [BUILD_MODULE]: buildModule };

registerHooks({
  resolve(specifier, context, next) {
    return Object.hasOwn(generated, specifier) ? { url: specifier, shortCircuit: true } : next(specifier, context);
  },
  load(url, context, next) {
    if (Object.hasOwn(generated, url)) {
      return { format: "module", source: generated[url](), shortCircuit: true };
    }
    if (url.startsWith("file:") && url.endsWith(".wasm")) {
      const source = `import { readFileSync } from "node:fs";\n`
        + `export default new WebAssembly.Module(readFileSync(new URL(${JSON.stringify(url)})));\n`;
      return { format: "module", source, shortCircuit: true };
    }
    return next(url, context);
  },
});
