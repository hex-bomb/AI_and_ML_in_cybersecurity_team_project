import Markdown from "react-markdown"
import remarkBreaks from "remark-breaks"
import remarkGfm from "remark-gfm"

import { cn } from "cn"

const SAFE_SCHEME = /^(https?:|mailto:)/i

/**
 * Markdown для ответов: заголовки, списки, код, таблицы.
 *
 * Текст и ссылки пишет LLM. react-markdown не вставляет HTML через
 * dangerouslySetInnerHTML, но href из ответа всё равно фильтруем по схеме:
 * javascript: и data: выполнились бы по клику.
 *
 * remark-breaks оставляет одиночный \n переносом строки — без него
 * CommonMark схлопывает переносы ответа в один абзац.
 */
export function MarkdownText({ children, className }: { children: string; className?: string }) {
  return (
    <div className={cn("space-y-2 text-sm leading-relaxed", className)}>
      <Markdown
        remarkPlugins={[remarkGfm, remarkBreaks]}
        components={{
          p: ({ children }) => <p className="whitespace-pre-wrap">{children}</p>,
          h1: ({ children }) => <h1 className="text-base font-semibold">{children}</h1>,
          h2: ({ children }) => <h2 className="text-sm font-semibold">{children}</h2>,
          h3: ({ children }) => <h3 className="text-sm font-semibold">{children}</h3>,
          ul: ({ children }) => <ul className="list-disc space-y-1 pl-5">{children}</ul>,
          ol: ({ children }) => <ol className="list-decimal space-y-1 pl-5">{children}</ol>,
          a: ({ children, href }) =>
            typeof href === "string" && SAFE_SCHEME.test(href.trim()) ? (
              <a
                href={href}
                target="_blank"
                rel="noopener noreferrer"
                className="underline underline-offset-2"
              >
                {children}
              </a>
            ) : (
              <span className="underline underline-offset-2">{children}</span>
            ),
          code: ({ children }) => (
            <code className="rounded-xl bg-muted px-1.5 py-0.5 font-mono text-xs">{children}</code>
          ),
          pre: ({ children }) => (
            <pre className="scroll-fade overflow-x-auto rounded-xl border border-border bg-muted/50 p-3 font-mono text-xs">
              {children}
            </pre>
          ),
          table: ({ children }) => (
            <div className="scroll-fade overflow-x-auto">
              <table className="w-full border-collapse text-xs">{children}</table>
            </div>
          ),
          th: ({ children }) => (
            <th className="border border-border px-2 py-1 text-left font-medium">{children}</th>
          ),
          td: ({ children }) => <td className="border border-border px-2 py-1">{children}</td>,
          blockquote: ({ children }) => (
            <blockquote className="border-l-2 border-border pl-3 text-muted-foreground">
              {children}
            </blockquote>
          ),
          strong: ({ children }) => <strong className="font-semibold">{children}</strong>,
          em: ({ children }) => <em className="italic">{children}</em>,
          hr: () => <hr className="border-border" />,
        }}
      >
        {children}
      </Markdown>
    </div>
  )
}
