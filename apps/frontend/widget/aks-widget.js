/**
 * AKS — загрузчик веб-виджета.
 *
 *   <script src="https://cdn.example.com/aks-widget.js" async></script>
 *
 * Скрипт только монтирует виджет во фрейме. Вся отрисовка, анимации,
 * перетаскивание и ресайз — внутри виджета (React), здесь ничего лишнего.
 *
 * Настройка через window.AKS_WIDGET до загрузки:
 *   {
 *     src: "/widget.html",
 *     apiUrl: "https://search.example/api/v1",
 *     kbId: "22222222-2222-4222-8222-222222222222",
 *     zIndex: 2147483000,
 *     widgetOrigin: "https://cdn.example.com",
 *     targetOrigin: "https://ваш-сайт.ру"
 *   }
 *
 * widgetOrigin и targetOrigin можно не задавать: первый выводится из src,
 * второй — из адреса самой страницы. Явно их пишут, когда виджет отдаётся
 * с CDN или страница открывается через прокси.
 *
 * Сессия живёт в httpOnly cookie на том же origin, что и API: ни в разметке
 * страницы, ни в postMessage токена нет. Хост после входа или выхода вызывает
 * `AKSWidget.configure()` — виджет перечитывает `/auth/me` сам.
 *
 * Если apiUrl и kbId заданы, виджет работает с настоящим API. Без них
 * остаётся демо-режим на моках, чтобы страницу можно было открыть без бэкенда.
 *
 * Контекст страницы собирается только по действию пользователя и только пока
 * панель открыта: адрес без query и fragment, заголовок, выделенный текст,
 * ближайшие data-assistant-content / data-assistant-section / data-assistant-version.
 * Поля ввода, скрытые блоки и всё под data-assistant-ignore не читаются.
 */
(function () {
  "use strict";

  function originOf(url) {
    try {
      return new URL(url, location.href).origin;
    } catch {
      return null;
    }
  }

  var CONFIG = window.AKS_WIDGET || {};
  var src = CONFIG.src || "/widget.html";
  // Исходящие сообщения идут только на свой origin: чужой документ,
  // оказавшийся в родительском окне, их не получит.
  var targetOrigin = CONFIG.targetOrigin || location.origin;
  // Входящие сверяем с origin виджета; по умолчанию он берётся из src.
  var widgetOrigin = CONFIG.widgetOrigin || originOf(src);
  var payload = {
    apiUrl: CONFIG.apiUrl || "",
    kbId: CONFIG.kbId || "",
    // Настоящий API включается только когда известны и адрес, и база.
    useMock: !(CONFIG.apiUrl && CONFIG.kbId),
  };

  var frame = document.createElement("iframe");
  frame.title = "Чат-бот";
  frame.setAttribute("data-aks-widget-frame", "");
  // Пока панель закрыта, попадания принимает только область кнопки: прозрачный
  // iframe во весь экран иначе перекрыл бы ссылки и кнопки самой страницы.
  // Регион совпадает с кнопкой виджета (60 px, отступ 24 px).
  var CLOSED_CLIP = "inset(calc(100% - 88px) 20px 20px calc(100% - 88px))";
  frame.style.cssText =
    "position:fixed;inset:0;width:100%;height:100%;border:0;background:transparent;" +
    "clip-path:" + CLOSED_CLIP + ";" +
    "z-index:" + (CONFIG.zIndex || 2147483000) + ";" +
    "color-scheme:normal;";

  // Фрейм должен подняться раньше отрисовки страницы, иначе он мигнёт.
  (document.body || document.documentElement).appendChild(frame);
  frame.src = src;

  function send(type) {
    if (frame.contentWindow) {
      var message = { source: "aks-host", type: type };
      if (type === "config") message.payload = payload;
      frame.contentWindow.postMessage(message, targetOrigin);
    }
  }

  var panelOpen = false;
  var lastUrl = location.href;

  function closestAttr(node, attribute) {
    var element = node && node.nodeType === 1 ? node : node && node.parentElement;
    var found = element && element.closest ? element.closest("[" + attribute + "]") : null;
    return found ? found.getAttribute(attribute) || "" : "";
  }

  /** Скрытые и исключённые контейнеры не читаем: там может быть что угодно. */
  function readable(element) {
    return !!element && element.getClientRects().length > 0 && !element.closest("[data-assistant-ignore]");
  }

  /** Текст выделения и ближайшая разрешённая разметка. null — контекста нет. */
  function collectContext() {
    if (!panelOpen) return null;

    var context = { url: location.href.split("#")[0], title: document.title || "" };
    var anchor = null;
    var selection = window.getSelection();

    if (selection && selection.rangeCount && !selection.isCollapsed) {
      anchor = selection.anchorNode;
      var element = anchor && anchor.nodeType === 1 ? anchor : anchor && anchor.parentElement;
      // Поля ввода и редактируемые области — личные данные, их не отправляем.
      if (!element || element.closest("input, textarea, [contenteditable='true']")) return context;
      if (!readable(element)) return context;

      var text = selection.toString().replace(/\s+/g, " ").trim().slice(0, 1000);
      if (text) context.selection = { text: text };
    }

    var bound = anchor
      ? (anchor.nodeType === 1 ? anchor : anchor.parentElement)
      : document.querySelector("[data-assistant-content]");
    var content = bound && bound.closest ? bound.closest("[data-assistant-content]") : null;
    if (content) {
      context.content_key = content.getAttribute("data-assistant-content") || "";
      context.version_hint = content.getAttribute("data-assistant-version") || "";
    }
    var section = bound && bound.closest ? bound.closest("[data-assistant-section]") : null;
    if (section) context.section_key = section.getAttribute("data-assistant-section") || "";

    return context;
  }

  /** Отправить контекст кадру; null — сброс (например, после SPA-перехода). */
  function sendContext(payload) {
    if (!frame.contentWindow) return;
    frame.contentWindow.postMessage(
      { source: "aks-host", type: "context", payload: payload || null },
      targetOrigin
    );
  }

  var contextTimer = null;

  function scheduleContext() {
    // Навигация меняет страницу целиком: старый контекст к ней не относится.
    if (location.href !== lastUrl) {
      lastUrl = location.href;
      sendContext(null);
    }
    if (!panelOpen) return;
    if (contextTimer) clearTimeout(contextTimer);
    contextTimer = setTimeout(function () {
      contextTimer = null;
      sendContext(collectContext());
    }, 150);
  }

  document.addEventListener("mouseup", scheduleContext, true);
  document.addEventListener("keyup", scheduleContext, true);
  window.addEventListener("popstate", function () {
    lastUrl = location.href;
    sendContext(null);
  });

  // SPA меняет адрес без popstate: иначе контекст прежней страницы остался бы.
  ["pushState", "replaceState"].forEach(function (name) {
    var original = history[name];
    if (typeof original !== "function") return;
    history[name] = function () {
      var result = original.apply(this, arguments);
      lastUrl = location.href;
      sendContext(null);
      return result;
    };
  });

  // Кадр сообщает, когда он готов принять конфиг: событие load срабатывает
  // раньше, чем React монтирует слушатель, и первый config терялся бы.
  function onFrameMessage(event) {
    var data = event.data;
    if (!data || data.source !== "aks-widget") return;
    if (event.source !== frame.contentWindow) return;
    // Кадр должен быть ровно тем, что мы загрузили, а не подменённым фреймом.
    if (widgetOrigin && event.origin !== widgetOrigin) return;
    // Виджет сообщает, открыта ли панель: пока закрыта — клики проходят мимо.
    if (data.type === "state" && typeof data.open === "boolean") {
      frame.style.clipPath = data.open ? "none" : CLOSED_CLIP;
      panelOpen = data.open;
      // Панель открыли — сразу отдаём контекст текущей страницы.
      if (panelOpen) sendContext(collectContext());
      return;
    }
    if (data.type === "ready") send("config");
  }
  window.addEventListener("message", onFrameMessage);
  frame.addEventListener("load", function () {
    send("config");
  });

  // Хост управляет виджетом через postMessage.
  window.addEventListener("message", function (event) {
    var data = event.data;
    if (!data || data.source !== "aks-host" || event.source !== window.parent) return;
    send(data.type);
  });

  window.AKSWidget = {
    open: function () {
      send("open");
    },
    close: function () {
      send("close");
    },
    toggle: function () {
      send("toggle");
    },
    /** Смена базы или адреса API на лету; также сигнал перечитать сессию. */
    configure: function (next) {
      payload.apiUrl = next.apiUrl || payload.apiUrl;
      payload.kbId = next.kbId || payload.kbId;
      payload.useMock = !(payload.apiUrl && payload.kbId);
      send("config");
    },
  };
})();
