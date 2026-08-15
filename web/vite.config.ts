import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";
import tailwindcss from "@tailwindcss/vite";

// The dev server proxies to a `glance serve` running on its default address.
//
// Going through the proxy rather than pointing the browser straight at :7717
// keeps the app same-origin in development, which is not cosmetic: the session
// cookie is `__Host-` prefixed and `SameSite=Strict`, so a cross-origin dev
// setup would never send it and every authenticated call would 401.
export default defineConfig({
  plugins: [react(), tailwindcss()],
  server: {
    port: 5173,
    proxy: {
      // `ws: true` covers /api/ws and /api/acp/agent; both are upgrades, and a
      // proxy that only forwards plain HTTP would fail the handshake.
      "/api": { target: "http://127.0.0.1:7717", ws: true },
    },
  },
  build: {
    // Emptying is what keeps a stale bundle from being embedded after a rename;
    // the Makefile restores web/dist/.gitkeep afterwards so `go build` still has
    // a directory to embed.
    emptyOutDir: true,
    // The server sets `Cache-Control: immutable` on hashed assets only, so
    // leaving the default hashed names in place is load-bearing.
    chunkSizeWarningLimit: 900,
  },
});
