import fs from "node:fs"
import { createRequire } from "node:module"
import path from "node:path"
import tailwindcss from "@tailwindcss/vite"
import react from "@vitejs/plugin-react"
import type { Plugin } from "vite"
import { defineConfig } from "vite"

const root = import.meta.dirname

/**
 * Каталог собранного виджета.
 *
 * Демо объявляет `@aks/widget` своей зависимостью и берёт артефакт оттуда, а
 * не из `../widget` руками: путь резолвится обычным node-резолвером, поэтому
 * демо не знает, где физически лежит пакет виджета. В npm workspaces симлинк
 * `@aks/widget` лежит в корне монорепозитория, а не в node_modules демо.
 */
function widgetDist(): string {
  // В пакете виджета нет "exports", поэтому package.json резолвится напрямую.
  const manifest = createRequire(import.meta.url).resolve("@aks/widget/package.json")
  return path.join(path.dirname(manifest), "dist")
}

const MOUNT = "/aks/"

/**
 * Виджет отдаётся демо ровно так, как его получил бы любой внешний сайт:
 * готовой статикой из каталога `/aks/`. Импортов исходников виджета в коде
 * демо нет — ни на dev-сервере, ни в собранном dist.
 *
 * Плагин только показывает готовый артефакт: ни vite build виджета, ни его
 * исходники сюда не попадают. Если сборки нет — понятная ошибка с командой.
 */
function widgetBundle(): Plugin {
  const dist = widgetDist()

  function requireBundle() {
    if (!fs.existsSync(path.join(dist, "widget.html"))) {
      throw new Error(
        `Виджет не собран: нет ${dist}/widget.html.\n` +
          "Соберите виджет командой: npm run build --workspace @aks/widget",
      )
    }
  }

  return {
    name: "aks-demo:widget-bundle",

    /** Проверяем до сборки: иначе после падения остался бы полупустой dist. */
    buildStart() {
      requireBundle()
    },

    /** Dev-сервер: отдаём файлы виджета из его каталога сборки. */
    configureServer(server) {
      server.middlewares.use((req, res, next) => {
        const url = (req.url ?? "").split("?")[0] ?? ""
        if (!url.startsWith(MOUNT)) return next()

        const file = path.join(dist, path.normalize(url.slice(MOUNT.length)))
        // Выход за пределы виджетной сборки — не наш путь.
        if (!file.startsWith(dist + path.sep) || !fs.existsSync(file)) {
          res.statusCode = 404
          res.end("404: нет такого файла виджета")
          return
        }

        res.setHeader("Content-Type", contentType(file))
        res.setHeader("Cache-Control", "no-store")
        fs.createReadStream(file).pipe(res)
      })
    },

    /** Production: копируем виджет в dist/aks, чтобы preview был рабочим. */
    closeBundle() {
      requireBundle()
      fs.cpSync(dist, path.resolve(root, "dist/aks"), { recursive: true })
    },
  }
}

function contentType(file: string): string {
  if (file.endsWith(".html")) return "text/html; charset=utf-8"
  if (file.endsWith(".js")) return "text/javascript; charset=utf-8"
  if (file.endsWith(".css")) return "text/css; charset=utf-8"
  if (file.endsWith(".svg")) return "image/svg+xml"
  if (file.endsWith(".png")) return "image/png"
  return "application/octet-stream"
}

export default defineConfig({
  plugins: [react(), tailwindcss(), widgetBundle()],
  resolve: {
    alias: {
      "@": path.resolve(root, "./src"),
    },
  },
  server: {
    port: 5174,
  },
})