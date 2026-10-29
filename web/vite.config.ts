import { fileURLToPath } from "node:url";
import tailwindcss from "@tailwindcss/vite";
import react from "@vitejs/plugin-react";
import { defineConfig } from "vite";

const webRoot = fileURLToPath(new URL(".", import.meta.url));

export default defineConfig(({ mode }) => {
  const demo = mode === "demo";
  return {
    root: webRoot,
    base: demo ? "/demo/" : "/",
    plugins: [tailwindcss(), react()],
    resolve: {
      alias: {
        "@": `${webRoot}/src`,
      },
    },
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
