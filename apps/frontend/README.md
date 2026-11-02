# Фронтенд AKS: два независимых пакета

В `apps/frontend/` лежат два пакета, которые собираются раздельно и не делят ни
исходников, ни сборку:

```text
apps/frontend/
  package.json          # npm workspaces: только оркестрация скриптов
  web-app/              # сайт-хост: лендинг «ВЕКТОР» + внедрение виджета
  widget/               # виджет: загрузчик, iframe-документ, весь общий код
  Dockerfile            # nginx со собранным web-app (виджет лежит в /aks)
```

## Почему так

Требование: виджет и сайт собираются независимо (у каждого свой `package.json`
и свой `vite build`), исходники виджета не дублируются, а сайт подключает
виджет **только загрузчиком**, как любой внешний сайт.

Выбран monorepo на npm workspaces с internal-зависимостью:

- `web-app/package.json` объявляет `"@aks/widget": "*"`. npm поднимает в корне
  `apps/frontend/node_modules` один набор пакетов и кладёт туда симлинк
  `node_modules/@aks/widget -> ../widget`.
- `web-app` **не импортирует исходники виджета**. Он берёт только собранный
  `widget/dist` и отдаёт его наружу как статику из `/aks/` — так же, как его
  отдавал бы внешний CDN. Плагин в `web-app/vite.config.ts` копирует (build)
  или отдаёт по запросу (dev) файлы из `node_modules/@aks/widget/dist`; без
  сборки падает с понятной командой, а не молча.
- `web-app/index.html` подключает виджет как внешний сайт:
  `<script src="/aks/aks-widget.js" async>` плюс `window.AKS_WIDGET` с `src`,
  `apiUrl` и `kbId`.

Почему не относительный путь `../widget/aks-widget.js`: зависимость в
`node_modules` не даёт собрать сайт без виджета, не привязывает сайт к
расположению виджета на диске и одинаково работает в dev, в production-сборке
и в `vite preview`.

Почему не импорт исходников виджета: это связало бы сборки и скопировало бы
весь виджет в бандл лендинга вместо одного iframe.

## Команды

Из `apps/frontend/` (поднимает оба пакета):

```bash
npm install
npm run build              # виджет, затем web-app
npm run typecheck          # tsc -b --noEmit в обоих пакетах
npm run lint               # oxlint в обоих пакетах
```

По пакетам — из своего каталога:

```bash
cd apps/frontend/widget && npm install && npm run build
cd apps/frontend/web-app && npm install && npm run build
```

Запуск для разработки (два терминала, сайт вторым — ему нужен `widget/dist`):

```bash
cd apps/frontend/widget  && npm run dev    # http://localhost:5175/widget.html
cd apps/frontend/web-app && npm run dev    # http://localhost:5173
```

## Как это устроено

- **Виджет** собирается с `base: "./"` и относительными ссылками на ассеты,
  поэтому `widget/dist` (`widget.html`, `widget.js`, `assets/`, `aks-widget.js`)
  можно положить под любой префикс — на хосте или на CDN. Загрузчик копируется
  в корень `dist` плагином `emit-loader`, весь `dist` — самодостаточный бандл.
- **Сайт** получает этот `dist` через `@aks/widget` и монтирует под `/aks/`.
  В собранном сайте это обычная статика рядом с лендингом, поэтому
  `npm run preview` показывает работающий виджет.
- **Iframe прозрачный и во весь экран**, поэтому пока панель закрыта, лоадер
  обрезает область попадания (`clip-path`) до кнопки виджета — иначе страница
  под ним не нажималась бы. Виджет сообщает хосту, когда панель открылась, и
  обрезка снимается.
- **Общение виджета с API** идёт через `apiUrl` из `window.AKS_WIDGET`. Адрес
  базы и режим моков тоже приходят от хоста. Сессии в `postMessage` нет: JWT
  лежит в httpOnly cookie, виджет спрашивает `/auth/me` сам — после загрузки и
  по команде `AKSWidget.configure()`, которую хост вызывает после входа или
  выхода. Роль определяет сервер из подписанного токена. Чаты живут в
  IndexedDB через dexie, отдельной базой на роль.
- **Картинки** `web-app/public/img/*.png` и `favicon.svg` — только для
  лендинга; в виджете статики нет.
- Фикс `dexie` + `EntityTable` живёт в `widget/tsconfig.app.json`: при
  `moduleResolution: "bundler"` dexie резолвится на свой `import-wrapper.d.mts`
  и теряет именованные типы, поэтому указан явный путь к `dexie.d.ts`. Путь
  задан двумя кандидатами, чтобы работать и в workspaces, и при одиночной
  установке в пакете.
