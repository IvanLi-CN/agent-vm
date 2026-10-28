import react from "@vitejs/plugin-react";
import { defineConfig } from "vite";

export default defineConfig(({ mode }) => {
  const demo = mode === "demo";
  return {
    root: "web",
    base: demo ? "/demo/" : "/",
    plugins: [react()],
    build: {
      outDir: demo ? "dist-demo" : "dist",
      emptyOutDir: true,
    },
    server: {
      port: demo ? 5174 : 5173,
      strictPort: false,
    },
  };
});
