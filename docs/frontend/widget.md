# widget

## Что делает

Встраиваемый чат: загрузчик `aks-widget.js` монтирует iframe, внутри — React-
приложение, которое общается с API по httpOnly cookie.

## Что внутри

- `widget.html` — документ кадра, собирается с `base: "./"`.
- `widget.js` + `assets/` — бандл приложения.
- `aks-widget.js` — загрузчик, копируется в корень `dist` плагином `emit-loader`.
- Чаты хранятся в IndexedDB (dexie), отдельной базой на роль.

## Контракт с хостом

`window.AKS_WIDGET` объявляется до подключения скрипта.

| Поле           | Обязательно        | Смысл                                            |
| -------------- | ------------------ | ------------------------------------------------ |
| `src`          | нет                | адрес `widget.html`, по умолчанию `/widget.html` |
| `apiUrl`       | для боевого режима | префикс API, обычно `/api/v1`                    |
| `kbId`         | для боевого режима | идентификатор базы знаний                        |
| `widgetOrigin` | нет                | origin виджета; выводится из `src`               |
| `targetOrigin` | нет                | origin хоста; по умолчанию `location.origin`     |
| `zIndex`       | нет                | слой iframe, по умолчанию `2147483000`           |

Без `apiUrl` и `kbId` виджет работает на локальных моках.

### Обмен сообщениями

| Кто         | Сообщение                                                   | Когда                         |
| ----------- | ----------------------------------------------------------- | ----------------------------- |
| хост → кадр | `{source: "aks-host", type: "config", payload}`             | после `ready` и `configure()` |
| хост → кадр | `{source: "aks-host", type: "open" \| "close" \| "toggle"}` | через `window.AKSWidget`      |
| хост → кадр | `{source: "aks-host", type: "context", payload}`            | по действию пользователя      |
| кадр → хост | `{source: "aks-widget", type: "ready"}`                     | после монтирования            |
| кадр → хост | `{source: "aks-widget", type: "state", open}`               | открытие и закрытие панели    |

Обе стороны проверяют `source`, `event.source` и origin. Загрузчик сверяет
ответы с `widgetOrigin`, кадр — команды с origin родителя: сначала берёт его из
`referrer`, затем запоминает из первого принятого сообщения. `targetOrigin = "*"`
не используется.

Версии у протокола нет: несовместимое изменение сломает уже встроенные виджеты.

## Контекст страницы

Загрузчик собирает контекст сам, без разметки на стороне хоста:

| Поле           | Откуда                                                        |
| -------------- | ------------------------------------------------------------- |
| `url`          | `location.href` без query и fragment                          |
| `title`        | `document.title`                                              |
| `selection`    | выделенный текст, до 1000 символов                            |
| `content_key`  | ближайший `[data-assistant-content]`, значение атрибута        |
| `version_hint` | `[data-assistant-version]` у того же контейнера                |
| `section_key`  | ближайший `[data-assistant-section]`                          |

Правила сбора:

- только когда панель открыта и только по действию пользователя (мышь, клавиши);
- содержимое `input`, `textarea`, `contenteditable` не читается;
- скрытые элементы и всё под `[data-assistant-ignore]` пропускается;
- после `pushState`, `replaceState` и `popstate` контекст сбрасывается.

Контекст виден в панели до отправки, и его можно убрать. Смена диалога
сбрасывает контекст: он относится к странице, а не к переписке.

Для явной привязки страницы к документу хост размечает контейнер:

```html
<main data-assistant-content="returns-policy" data-assistant-version="7">
  <section data-assistant-section="discounted-items">...</section>
</main>
```

Сервер считает `content_key` подсказкой для поиска, а не разрешением на чтение:
доступ по-прежнему определяется правами сессии.

## Ответы модели

Текст рендерит `react-markdown`, без `dangerouslySetInnerHTML`. Ссылки
дополнительно фильтруются по схеме: `http`, `https`, `mailto`. Остальные
показываются текстом, чтобы `javascript:` и `data:` не сработали по клику.

## Ошибки

`describeError` в `chatStore` переводит коды API в текст для пользователя.
Список кодов — в [справочнике API](../reference/api.md#коды-ошибок). Технический
`request_id` показывается рядом с текстом ошибки; с ним ищут запрос в логах.

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
