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
 *     targetOrigin: "https://ваш-сайт.ру"
 *   }
 *
 * Сессия живёт в httpOnly cookie на том же origin, что и API: ни в разметке
 * страницы, ни в postMessage токена нет. Хост после входа или выхода вызывает
 * `AKSWidget.configure()` — виджет перечитывает `/auth/me` сам.
 *
 * Если apiUrl и kbId заданы, виджет работает с настоящим API. Без них
 * остаётся демо-режим на моках, чтобы страницу можно было открыть без бэкенда.
 */
(function () {
  "use strict";

  var CONFIG = window.AKS_WIDGET || {};
  var src = CONFIG.src || "/widget.html";
  // "*" по умолчанию: виджет и хост часто на разных доменах, а кадр сам
  // выставляет origin. Если домен известен — передайте его, и тогда сообщения
  // не уйдут ни к какому другому получателю.
  var targetOrigin = CONFIG.targetOrigin || "*";
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

  // Кадр сообщает, когда он готов принять конфиг: событие load срабатывает
  // раньше, чем React монтирует слушатель, и первый config терялся бы.
  function onFrameMessage(event) {
    var data = event.data;
    if (!data || data.source !== "aks-widget") return;
    if (event.source !== frame.contentWindow) return;
    // Виджет сообщает, открыта ли панель: пока закрыта — клики проходят мимо.
    if (data.type === "state" && typeof data.open === "boolean") {
      frame.style.clipPath = data.open ? "none" : CLOSED_CLIP;
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
