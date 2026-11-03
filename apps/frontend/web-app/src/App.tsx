import { DemoShowcase } from "@/pages/DemoShowcase"
import "@/styles/globals.css"

/**
 * Страница-хост лендинга. Виджета здесь нет: его монтирует загрузчик из
 * index.html тем же способом, что и на любом внешнем сайте.
 *
 * Адрес API и база знаний объявлены там же, до подключения загрузчика:
 * async-скрипт читает window.AKS_WIDGET сразу при запуске.
 */
export default function App() {
  return <DemoShowcase />
}
