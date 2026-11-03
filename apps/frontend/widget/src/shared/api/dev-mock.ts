import type { AnswerResponse, SearchHit } from "../types/api"

/**
 * Локальная заглушка API: включается флагом `useMock`, который ставит
 * aks-widget.js, когда не заданы apiUrl и kbId. В боевом режиме сюда не заходят.
 *
 * DTO совпадают с боевыми, поэтому компоненты не знают, откуда пришёл ответ
 * (ТЗ 10.2, шаг 5).
 */

const FIXTURES: Array<{ question: string; answer: string; hits: SearchHit[] }> = [
  {
    question: "",
    answer:
      "Откройте раздел «Интеграции» и настройте параметры выгрузки [1].\n\nПосле этого запустите пробную выгрузку — она не изменит данные в системе.",
    hits: [
      {
        chunk_id: "sha256:cccccccc000000000000000000000000000000000000000000000000000000001",
        document_id: "33333333-3333-4333-8333-333333333333",
        version_id: "44444444-4444-4444-8444-444444444444",
        title: "Инструкция по выгрузке",
        text: "Для настройки выгрузки откройте раздел Интеграции и укажите получателя.",
        locator: {
          block_ids: ["55555555-5555-4555-8555-555555555555"],
          section_path: ["Настройка", "Выгрузка"],
          page_start: 3,
          page_end: 3,
        },
        source_url: "https://portal.example/guide/export",
        download_url:
          "/api/v1/documents/33333333-3333-4333-8333-333333333333/source?version_id=44444444-4444-4444-8444-444444444444",
        score: 0.032,
      },
      {
        chunk_id: "sha256:cccccccc000000000000000000000000000000000000000000000000000000002",
        document_id: "33333333-3333-4333-8333-333333333334",
        version_id: "44444444-4444-4444-8444-444444444445",
        title: "Регламент обслуживания",
        text: "Запросы на выгрузку обрабатываются в порядке поступления, до 25 запросов в час.",
        locator: {
          block_ids: ["55555555-5555-4555-8555-555555555556"],
          section_path: ["Регламент"],
          page_start: 1,
          page_end: 1,
        },
        source_url: null,
        download_url:
          "/api/v1/documents/33333333-3333-4333-8333-333333333334/source?version_id=44444444-4444-4444-8444-444444444445",
        score: 0.021,
      },
    ],
  },
]

/** Подбирает фикстур по слову из вопроса, иначе берёт первый. */
function pick(question: string) {
  const lower = question.toLowerCase()
  return (
    FIXTURES.find((f) => f.hits.some((h) => lower.includes(h.title.toLowerCase()))) ?? FIXTURES[0]!
  )
}

export async function mockAnswer(question: string): Promise<AnswerResponse> {
  await delay(900 + Math.random() * 600)

  // Пустой вопрос — сервер не вызывает LLM и отвечает no_evidence.
  if (!question.trim()) {
    return {
      status: "no_evidence",
      answer: "В доступной базе знаний не найдено подтверждающих материалов.",
      citations: [],
      request_id: crypto.randomUUID(),
    }
  }

  const fixture = pick(question)
  return {
    status: "ok",
    answer: fixture.answer,
    citations: fixture.hits.map((hit, index) => ({
      number: index + 1,
      chunk_id: hit.chunk_id,
      document_id: hit.document_id,
      version_id: hit.version_id,
      title: hit.title,
      quote: hit.text,
      locator: hit.locator,
      source_url: hit.source_url,
      download_url: hit.download_url,
    })),
    request_id: crypto.randomUUID(),
  }
}

function delay(ms: number) {
  return new Promise((resolve) => setTimeout(resolve, ms))
}
