import fs from "node:fs"
import path from "node:path"
import tailwindcss from "@tailwindcss/vite"
import react from "@vitejs/plugin-react"
import type { Plugin } from "vite"
import { defineConfig } from "vite"

const root = import.meta.dirname
const LOADER = path.resolve(root, "aks-widget.js")

/**
 * Загрузчик — обычный статический файл, а не модуль виджета: его копируют на
 * внешний сайт как есть. Поэтому после сборки копируем его в корень dist,
 * и весь dist становится самодостаточным «CDN-бандлом»:
 * aks-widget.js + widget.html + widget.js + assets.
 */
function emitLoader(): Plugin {
  return {
    name: "aks-widget:emit-loader",
    apply: "build",
    closeBundle() {
      fs.copyFileSync(LOADER, path.resolve(root, "dist/aks-widget.js"))
    },
  }
}

export default defineConfig({
  // Относительные ссылки на ассеты: виджет кладут в iframe на site-хосте по
  // любому префиксу (/widget.html, /assets/aks/widget.html, CDN-путь), и
  // абсолютный '/' сломал бы такой хостинг.
  base: "./",
  plugins: [react(), tailwindcss(), emitLoader()],
  resolve: {
    alias: {
      "@": path.resolve(root, "./src"),
      // shadcn registry files import the cn helper as a bare specifier.
      cn: path.resolve(root, "./src/lib/utils.ts"),
    },
  },
  server: {
    port: 5175,
  },
  build: {
    outDir: "dist",
    emptyOutDir: true,
    rollupOptions: {
      input: path.resolve(root, "widget.html"),
      output: {
        entryFileNames: "widget.js",
        chunkFileNames: "assets/[name]-[hash].js",
        assetFileNames: "assets/[name]-[hash][extname]",
      },
    },
  },
})