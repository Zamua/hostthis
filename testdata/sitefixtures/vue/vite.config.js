import { defineConfig } from "vite";
import vue from "@vitejs/plugin-vue";

// Relative base so the built bundle loads under any <slug>.hostthis.dev
// subdomain. Deterministic build output: vite content-hashes asset
// names, so identical source yields identical dist bytes (a stable
// fixture).
export default defineConfig({
  base: "./",
  plugins: [vue()],
  build: {
    chunkSizeWarningLimit: 1000,
  },
});
