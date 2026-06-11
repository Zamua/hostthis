import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";

// Relative base so the built bundle loads under any <slug>.hostthis.dev
// subdomain without a per-deploy base path. Deterministic build output:
// vite content-hashes asset names, so identical source yields identical
// dist bytes, which is what makes the committed dist/ a stable fixture.
export default defineConfig({
  base: "./",
  plugins: [react()],
  build: {
    // Keep the chunk layout flat and predictable for the fixture.
    chunkSizeWarningLimit: 1000,
  },
});
