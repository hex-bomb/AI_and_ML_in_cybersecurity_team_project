import { createRoot } from "react-dom/client"

import WidgetPage from "@/pages/WidgetPage"

/**
 * Точка входа виджета. Собирается отдельным бандлом (widget.js) и поднимается
 * загрузчиком aks-widget.js в iframe на странице-хосте.
 */
const container = document.createElement("div")
container.id = "aks-widget-root"
container.dataset.widgetRoot = "true"
container.style.cssText = "height:100%;width:100%"
document.body.append(container)

createRoot(container).render(<WidgetPage />)