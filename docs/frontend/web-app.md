# web-app

## Что делает

Лендинг «ВЕКТОР» и страница-хост, которая встраивает виджет чата ровно так же,
как это делал бы внешний сайт.

## Что внутри

- React 19, Vite, Tailwind CSS.
- Плагин `widgetBundle` берёт собранный `widget/dist` через `@aks/widget` и
  отдаёт его из `/aks/`.
- `index.html` объявляет `window.AKS_WIDGET` и подключает `/aks/aks-widget.js`.
- Цвет акцента — токен `--brand` в `src/styles/globals.css`; те же значения
  продублированы в виджете, потому что у него свой CSS в iframe.

## Запуск

```bash
cd apps/frontend
npm install
npm run build:widget
npm run dev:web-app       # http://localhost:5173
```

Прокси `/api` на `localhost:8080` настроен в `vite.config.ts`.

## Сборка

```bash
npm run build             # widget, затем web-app
```

Результат — статика в `web-app/dist`, её отдаёт nginx из
`apps/frontend/Dockerfile`.

## Зависимости

Пакет `@aks/widget` (workspace). Без собранного виджета сборка падает с
подсказкой.

## Подробнее

[../reference/api.md](../reference/api.md),
[widget.md](widget.md)
