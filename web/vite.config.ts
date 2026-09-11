import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";
import tailwindcss from "@tailwindcss/vite";

// The bundle is emitted straight into the Go package that embeds it, so
// `make ui` is the only step between a source change and a binary that
// carries it. Nothing copies the build product around afterwards, because a
// copy is a thing that can be stale.
export default defineConfig({
  plugins: [react(), tailwindcss()],
  resolve: { alias: { "@": new URL("./src", import.meta.url).pathname } },
  build: { outDir: "../pkg/server/web_dist", emptyOutDir: true },
  server: {
    port: 43520,
    // In development the app runs from Vite and the API from the Go server,
    // so the cookie has to be same-origin: proxy rather than CORS, which
    // would need the session cookie to be SameSite=None and give up the one
    // property that stops CSRF against every write route.
    proxy: Object.fromEntries(
      ["/api", "/athanor", "/brain", "/graph", "/v1", "/ui", "/metrics"].map((p) => [
        p,
        { target: "http://127.0.0.1:47832", changeOrigin: false },
      ]),
    ),
  },
});
