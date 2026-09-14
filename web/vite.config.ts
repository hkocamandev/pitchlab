// defineConfig comes from vitest/config rather than vite so the test block is
// typed. It is the same function with the test options added.
import { defineConfig } from "vitest/config";
import react from "@vitejs/plugin-react";

// The dev server proxies the API rather than having the app point at
// localhost:8081 directly. Same-origin in development means the browser sends
// no Origin header the server has to allow, and the production build -- served
// by nginx in front of the same API -- behaves identically. A base URL in the
// client would have to be configured differently in each, which is how a
// dashboard ends up working in development and not in a container.
export default defineConfig({
  plugins: [react()],
  build: {
    rollupOptions: {
      output: {
        // Recharts is most of the bundle and it changes far less often than
        // the application does. Splitting it means a deploy invalidates the
        // app chunk and leaves the charting library cached.
        manualChunks: { charts: ["recharts"] },
      },
    },
  },
  server: {
    port: 5173,
    proxy: {
      "/api": { target: "http://localhost:8081", changeOrigin: true },
      "/ws": { target: "ws://localhost:8081", ws: true },
      "/readyz": "http://localhost:8081",
    },
  },
  test: {
    globals: true,
    environment: "jsdom",
    setupFiles: ["./src/test/setup.ts"],
    css: false,
  },
});
