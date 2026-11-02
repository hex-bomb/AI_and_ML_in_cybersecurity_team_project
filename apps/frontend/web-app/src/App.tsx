import { DemoShowcase } from "@/pages/DemoShowcase"
import "@/styles/globals.css"

/**
 * Страница-хост.
 *
 * Виджет подключается ровно так же, как на любом внешнем сайте — одним тегом
 * <script src="/aks-widget.js" async> в index.html. Здесь только состояние
 * чата, чтобы инлайн-предпросмотр на странице тоже работал.
 *
 * Адрес API и база знаний объявлены в index.html до подключения загрузчика:
 * скрипт async читает window.AKS_WIDGET сразу при запуске.
 */
export default function App() {
  return <DemoShowcase />
}