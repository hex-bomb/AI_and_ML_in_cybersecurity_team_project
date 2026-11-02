# widget

## Что делает

Встраиваемый чат: загрузчик `aks-widget.js` монтирует iframe, внутри — React
приложение, которое общается с API по httpOnly cookie.

## Что внутри

- `widget.html` — документ кадра, собирается с `base: "./"`.
- `widget.js` + `assets/` — бандл приложения.
- `aks-widget.js` — загрузчик, копируется в корень `dist` плагином `emit-loader`.
- Чаты хранятся в IndexedDB (dexie), отдельной базой на роль.

## Контракт с хостом

`window.AKS_WIDGET` до подключения скрипта:

| Поле           | Смысл                                            |
| -------------- | ------------------------------------------------ |
| `src`          | адрес `widget.html`                              |
| `apiUrl`       | префикс API, обычно `/api/v1`                    |
| `kbId`         | идентификатор базы знаний                        |
| `targetOrigin` | origin хоста для `postMessage`                   |
| `zIndex`       | слой iframe                                      |

Без `apiUrl` и `kbId` виджет работает на моках.

## Запуск

```bash
cd apps/frontend
npm install
npm run dev:widget        # http://localhost:5175/widget.html
```

## Сборка

```bash
npm run build:widget      # dist: widget.html, widget.js, assets/, aks-widget.js
```

`dist` самодостаточен: его можно положить на любой префикс или в CDN.

## Подробнее

[web-app.md](web-app.md),
[../reference/api.md](../reference/api.md)
