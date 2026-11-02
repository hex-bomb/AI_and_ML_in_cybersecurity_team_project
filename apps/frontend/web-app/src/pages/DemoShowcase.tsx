import { useRef } from "react"
import {
  motion,
  useReducedMotion,
  useScroll,
  useSpring,
  useTransform,
} from "motion/react"
import { ArrowRightIcon } from "lucide-react"

import { AuthMenu } from "@/components/AuthMenu"

/**
 * Лендинг вымышленной компании «ВЕКТОР» — поставка строительной и
 * промышленной техники.
 *
 * Оформление по методике Apple (skill `apple-design`), а не по референсу
 * Revolut, которому страница следовала раньше. Что это меняет:
 *
 * - Типографика задаёт иерархию размером, весом и интерлиньяжем вместе: у
 *   крупных заголовков плотный интерлиньяж и отрицательный трекинг, у текста
 *   — свободный. Заголовки в обычном регистре, а не капсом.
 * - Движение на пружинах: `bounce: 0, duration: 0.4` — критическое
 *   демпфирование без перелёта. Появление блоков идёт кросс-фейдом.
 * - Полупрозрачные материалы вместо линеек: шапка размывает фон, а под ней
 *   градиентом гаснет контент — без единой границы.
 * - Отклик мгновенный: кнопка уменьшается на нажатии, а не после клика.
 * - `prefers-reduced-motion` превращает сдвиги в простые фейды.
 */

type Shot = { src: string; alt: string }

type CatalogItem = Shot & { eyebrow: string; title: string; text: string; scale?: number }

const CATALOG: CatalogItem[] = [
  {
    eyebrow: "Станки",
    title: "Точность до микрона",
    text: "Токарные и фрезерные станки с числовым управлением. Геометрия детали повторяется от заготовки к заготовке.",
    src: "/img/lathe.png",
    alt: "Токарно-фрезерный станок с ЧПУ",
  },
  {
    eyebrow: "Земляные работы",
    title: "Копаем быстрее",
    text: "Гусеничные экскаваторы от 8 тонн. Гидравлика с ресурсом до 12 000 часов до капитального ремонта.",
    src: "/img/excavator.png",
    alt: "Гусеничный экскаватор",
  },
  {
    eyebrow: "Сварочные работы",
    title: "Ровный шов с первого прохода",
    text: "Промышленные роботы-манипуляторы. Повторяемость осциллограмм — до 0,05 мм.",
    src: "/img/robot.png",
    alt: "Промышленный робот-манипулятор",
    // Рука робота уходит вверх, поэтому в ряду он кажется тяжелее остальных.
    scale: 0.82,
  },
  {
    eyebrow: "Стройплощадка",
    title: "Всё на одном объекте",
    text: "Башенные краны, бетоносмесители, генераторы. Поставка комплектом, пусконаладка на площадке.",
    src: "/img/site.png",
    alt: "Башенный кран и бетоносмеситель",
    // Кран с бетоносмесителем — самая высокая композиция, уменьшаем.
    scale: 0.78,
  },
]

const HERO_SHOT: Shot = {
  src: "/img/lathe.png",
  alt: "Токарно-фрезерный станок с числовым управлением",
}

const STATS = [
  { value: "1 240", label: "единиц техники поставлено" },
  { value: "18 лет", label: "на рынке промышленного оборудования" },
  { value: "48 ч", label: "гарантия сервисного выезда" },
  { value: "0,05 мм", label: "повторяемость роботизированной сварки" },
]

const SERVICES = [
  {
    title: "Подменный фонд",
    text: "Пока ремонтируем ваш станок, вы работаете на подменном.",
  },
  {
    title: "Инженер по участку",
    text: "Закреплённый специалист знает парк не по документам.",
  },
  {
    title: "Склад запчастей",
    text: "18 000 позиций в наличии, отгрузка в день заявки.",
  },
  {
    title: "Обучение операторов",
    text: "Готовим персонал под конкретную модель станка.",
  },
]

const NAV = [
  { label: "Каталог", href: "#catalog" },
  { label: "Сервис", href: "#service" },
  { label: "Лизинг", href: "#leasing" },
  { label: "Контакты", href: "#contacts" },
]

/** Пружина по умолчанию: критическое демпфирование, без перелёта. */
const SPRING = { type: "spring", bounce: 0, duration: 0.4 } as const

/** Пружина для импульсных взаимодействий — с небольшим перелётом. */
const SPRING_BOUNCY = { type: "spring", bounce: 0.2, duration: 0.4 } as const

/** Акцентный синий Apple — для ссылок и основных действий. */
const ACCENT = "text-[#0071e3] hover:text-[#0077ed]"
const ACCENT_BG = "bg-[#0071e3] hover:bg-[#0077ed]"

export function DemoShowcase() {
  return (
    <div className="min-h-dvh bg-background text-foreground antialiased">
      <Nav />
      <Hero />
      <Catalog />
      <Service />
      <Stats />
      <Leasing />
      <Footer />
    </div>
  )
}

/* шапка */

function Nav() {
  const { scrollY } = useScroll()

  // Фон шапки размыт всегда и сильно: это стекло, а не плашка. По мере
  // прокрутки добавляется только лёгкое затемнение — ровно настолько, чтобы
  // текст читался, но размытие оставалось видимым.
  const progress = useSpring(scrollY, { stiffness: 160, damping: 30, mass: 0.4 })
  const scrim = useTransform(progress, [0, 240], [0, 0.35])

  return (
    <header className="fixed inset-x-0 top-0 z-40 bg-background/45 backdrop-blur-3xl backdrop-saturate-150">
      <motion.div
        className="absolute inset-0 -z-10 bg-background"
        style={{ opacity: scrim }}
      />
      {/* Размытая кромка вместо границы — конент уходит под плавающую шапку. */}
      <div className="pointer-events-none absolute inset-x-0 top-full h-6 bg-gradient-to-b from-background to-transparent" />

      <div className="mx-auto flex h-12 max-w-[1080px] items-center gap-8 px-6">
        <a
          href="#top"
          className={`text-[17px] font-semibold tracking-tight transition-opacity active:opacity-60 ${ACCENT}`}
        >
          ВЕКТОР
        </a>

        <nav className="ml-auto hidden items-center gap-8 text-[13px] md:flex">
          {NAV.map((item) => (
            <a
              key={item.href}
              href={item.href}
              className={`transition-opacity active:opacity-60 ${ACCENT}`}
            >
              {item.label}
            </a>
          ))}
        </nav>

        <div className="ml-auto flex items-center gap-2 md:ml-0">
          <motion.a
            href="#contacts"
            whileTap={{ scale: 0.97 }}
            transition={SPRING_BOUNCY}
            className={`rounded-full px-4 py-1.5 text-[13px] font-medium text-white transition-colors active:opacity-80 ${ACCENT_BG}`}
          >
            Получить цену
          </motion.a>

          {/* Вход в базу знаний: сессия принадлежит странице, не виджету. */}
          <AuthMenu />
        </div>
      </div>
    </header>
  )
}

/* первый экран */

function Hero() {
  const ref = useRef<HTMLElement>(null)
  const reduced = useReducedMotion()
  const { scrollYProgress } = useScroll({ target: ref, offset: ["start start", "end start"] })

  // Техника уходит вверх и слегка уменьшается; текст — наоборот, догоняет.
  const shotY = useTransform(scrollYProgress, [0, 1], [0, -120])
  const shotScale = useTransform(scrollYProgress, [0, 1], [1, 0.94])
  const copyY = useTransform(scrollYProgress, [0, 1], [0, 44])

  return (
    <section
      id="top"
      ref={ref}
      className="flex min-h-dvh flex-col justify-center px-6 pt-24 pb-16"
    >
      <div className="mx-auto w-full max-w-[1080px]">
        <motion.div style={reduced ? undefined : { y: copyY }} className="mx-auto max-w-[820px] text-center">
          <Reveal delay={0.05}>
            <Eyebrow>Промышленные станки</Eyebrow>
          </Reveal>

          <Reveal delay={0.12}>
            <h1 className="mt-4 text-[clamp(2.4rem,6.4vw,4.6rem)] leading-[1.04] font-semibold tracking-[-0.025em] text-balance">
              Техника, которая
              <br />
              держит размер
            </h1>
          </Reveal>

          <Reveal delay={0.2}>
            <p className="mx-auto mt-6 max-w-[620px] text-[19px] leading-[1.5] text-muted-foreground text-balance">
              Токарные, фрезерные, сварочные и землеройные машины с точностью под
              задачу. Поставляем, пускуем и обслуживаем в одном договоре.
            </p>
          </Reveal>

          <Reveal delay={0.28}>
            <div className="mt-8 flex flex-wrap items-center justify-center gap-3">
              <motion.a
                href="#catalog"
                whileTap={{ scale: 0.97 }}
                transition={SPRING_BOUNCY}
                className={`rounded-full px-6 py-3 text-[15px] font-medium text-white transition-colors active:opacity-80 ${ACCENT_BG}`}
              >
                Смотреть каталог
              </motion.a>
              <motion.a
                href="#leasing"
                whileTap={{ scale: 0.97 }}
                transition={SPRING_BOUNCY}
                className={`flex items-center gap-1 rounded-full px-6 py-3 text-[15px] font-medium transition-colors active:opacity-80 ${ACCENT}`}
              >
                Лизинг под 3%
                <ArrowRightIcon className="size-4" />
              </motion.a>
            </div>
          </Reveal>
        </motion.div>

        {/* Материализация: размытие и масштаб приходят вместе, поэтому
            поверхность выглядит появляющейся, а не просто проявляющейся. */}
        <motion.div
          style={reduced ? undefined : { y: shotY, scale: shotScale }}
          className="mt-14 origin-top"
          initial={reduced ? { opacity: 0 } : { opacity: 0, scale: 0.97, filter: "blur(12px)" }}
          animate={reduced ? { opacity: 1 } : { opacity: 1, scale: 1, filter: "blur(0px)" }}
          transition={reduced ? { duration: 0.4 } : SPRING}
        >
          <Shot src={HERO_SHOT.src} alt={HERO_SHOT.alt} />
        </motion.div>
      </div>
    </section>
  )
}

/* каталог */

function Catalog() {
  return (
    <section id="catalog" className="px-6 pt-24 pb-8">
      <div className="mx-auto max-w-[1080px]">
        <div className="max-w-[760px]">
          <Eyebrow>Каталог</Eyebrow>
          <Heading>Четыре линейки, одна точность</Heading>
        </div>

        <div className="mt-16 flex flex-col gap-24 sm:gap-32">
          {CATALOG.map((item, index) => (
            <CatalogRow key={item.title} item={item} flip={index % 2 === 1} />
          ))}
        </div>
      </div>
    </section>
  )
}

function CatalogRow({ item, flip }: { item: CatalogItem; flip: boolean }) {
  const ref = useRef<HTMLDivElement>(null)
  const reduced = useReducedMotion()
  const { scrollYProgress } = useScroll({ target: ref, offset: ["start 0.9", "start 0.45"] })

  const shotY = useTransform(scrollYProgress, [0, 1], [48, 0])
  const copyY = useTransform(scrollYProgress, [0, 1], [32, 0])
  const opacity = useTransform(scrollYProgress, [0, 0.8], [0, 1])

  return (
    <div
      ref={ref}
      className={`grid items-center gap-10 lg:grid-cols-2 lg:gap-16 ${flip ? "lg:[&>*:first-child]:order-2" : ""}`}
    >
      <motion.div style={reduced ? { opacity: 1 } : { y: shotY, opacity }}>
        <Shot src={item.src} alt={item.alt} scale={item.scale} />
      </motion.div>

      <motion.div style={reduced ? { opacity: 1 } : { y: copyY, opacity }}>
        <Eyebrow>{item.eyebrow}</Eyebrow>
        <h3 className="mt-3 text-[clamp(1.75rem,3.4vw,2.6rem)] leading-[1.08] font-semibold tracking-[-0.02em] text-balance">
          {item.title}
        </h3>
        <p className="mt-4 max-w-[460px] text-[17px] leading-[1.5] text-muted-foreground">
          {item.text}
        </p>
      </motion.div>
    </div>
  )
}

/* сервис */

function Service() {
  return (
    <section id="service" className="px-6 py-24 sm:py-32">
      <div className="mx-auto max-w-[1080px]">
        <div className="max-w-[760px]">
          <Eyebrow>Сервис</Eyebrow>
          <Heading>Выезжаем туда, где сломалась машина</Heading>
        </div>

        <div className="mt-14 grid gap-4 sm:grid-cols-2 lg:grid-cols-4">
          {SERVICES.map((item, index) => (
            <Reveal key={item.title} delay={index * 0.06}>
              <article className="h-full rounded-2xl bg-secondary/60 p-6">
                <h3 className="text-[17px] font-semibold tracking-[-0.01em]">{item.title}</h3>
                <p className="mt-2 text-[15px] leading-[1.5] text-muted-foreground">{item.text}</p>
              </article>
            </Reveal>
          ))}
        </div>
      </div>
    </section>
  )
}

/* цифры */

function Stats() {
  return (
    <section className="px-6 py-20 sm:py-28">
      <div className="mx-auto max-w-[1080px]">
        <div className="grid gap-10 border-t border-border pt-14 sm:grid-cols-2 lg:grid-cols-4">
          {STATS.map((item, index) => (
            <Reveal key={item.label} delay={index * 0.06}>
              <div className="text-[clamp(2rem,4vw,2.9rem)] leading-none font-semibold tracking-[-0.02em] tabular-nums">
                {item.value}
              </div>
              <p className="mt-3 max-w-[200px] text-[15px] leading-[1.5] text-muted-foreground">
                {item.label}
              </p>
            </Reveal>
          ))}
        </div>
      </div>
    </section>
  )
}

/* лизинг */

function Leasing() {
  return (
    <section id="leasing" className="px-6 py-24 sm:py-32">
      <div className="mx-auto max-w-[1080px]">
        <div className="grid items-end gap-10 lg:grid-cols-[1.15fr_1fr] lg:gap-20">
          <div>
            <Eyebrow>Лизинг</Eyebrow>
            <Heading>Станок в работе за 48 часов</Heading>
          </div>

          <div>
            <p className="text-[17px] leading-[1.5] text-muted-foreground">
              Собственное производство и проверенные дилеры. Считаем лизинг за
              день, аванс от 10%, ставка от 3% годовых на срок до 36 месяцев.
            </p>
            <motion.a
              href="#contacts"
              whileTap={{ scale: 0.97 }}
              transition={SPRING_BOUNCY}
              className={`mt-6 inline-flex items-center gap-1.5 rounded-full px-6 py-3 text-[15px] font-medium text-white transition-colors active:opacity-80 ${ACCENT_BG}`}
            >
              Рассчитать лизинг
              <ArrowRightIcon className="size-4" />
            </motion.a>
          </div>
        </div>
      </div>
    </section>
  )
}

/* подвал */

function Footer() {
  return (
    <footer id="contacts" className="border-t border-border px-6 py-10">
      <div className="mx-auto flex max-w-[1080px] flex-col gap-8 md:flex-row md:items-end md:justify-between">
        <div>
          <div className="text-[17px] font-semibold tracking-tight text-[#0071e3]">ВЕКТОР</div>
          <p className="mt-3 max-w-[380px] text-[13px] leading-[1.5] text-muted-foreground">
            Поставка строительной и промышленной техники с 2008 года.
            Демонстрационный проект — компания вымышленная.
          </p>
        </div>

        <div className="flex flex-col gap-1.5 text-[13px] md:flex-row md:gap-6">
          {NAV.map((item) => (
            <a key={item.href} href={item.href} className={`transition-opacity active:opacity-60 ${ACCENT}`}>
              {item.label}
            </a>
          ))}
          <span className="text-muted-foreground">Екатеринбург, Уральская 12</span>
        </div>
      </div>
    </footer>
  )
}

/* общие части */

/**
 * Появление блока: подъём и проявление на пружине.
 *
 * При `prefers-reduced-motion` остаётся только фейд — без сдвига, чтобы не
 * провоцировать укачивание.
 */
function Reveal({
  children,
  delay = 0,
  className,
}: {
  children: React.ReactNode
  delay?: number
  className?: string
}) {
  const reduced = useReducedMotion()

  return (
    <motion.div
      className={className}
      initial={reduced ? { opacity: 0 } : { opacity: 0, y: 24 }}
      whileInView={reduced ? { opacity: 1 } : { opacity: 1, y: 0 }}
      viewport={{ once: true, margin: "-12% 0px" }}
      transition={reduced ? { duration: 0.4 } : { ...SPRING, delay }}
    >
      {children}
    </motion.div>
  )
}

/** Заголовок секции: плотный интерлиньяж и отрицательный трекинг. */
function Heading({ children }: { children: React.ReactNode }) {
  return (
    <h2 className="mt-3 text-[clamp(1.9rem,4vw,3.1rem)] leading-[1.06] font-semibold tracking-[-0.022em] text-balance">
      {children}
    </h2>
  )
}

/** Надзаголовок — приглушённый, мелкий, с положительным трекингом. */
function Eyebrow({ children }: { children: React.ReactNode }) {
  return (
    <p className="text-[13px] font-medium tracking-[0.02em] text-muted-foreground">{children}</p>
  )
}

/**
 * Техника на белом холсте.
 *
 * Полутоновый стиль отключён: машины показываются обычными снимками. Рамка и
 * растр лежат готовыми в `entities/tech/MachineShot` — вернутся одной
 * заменой этого компонента.
 *
 * `scale` уменьшает отдельные кадры: робот и кран выше ростом и без этого
 * перевешивают соседей по ряду.
 */
function Shot({ src, alt, scale }: Shot & { scale?: number }) {
  return (
    <img
      src={src}
      alt={alt}
      draggable={false}
      loading="lazy"
      // Уменьшенную картинку центрируем, иначе она прижмётся к левому краю.
      style={scale ? { width: `${scale * 100}%`, marginInline: "auto" } : undefined}
      className="block h-auto w-full select-none"
    />
  )
}
