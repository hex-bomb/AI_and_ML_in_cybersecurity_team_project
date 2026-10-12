#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""Установка SKILLS из ECC в глобальную область Zed (~/.agents/skills/).

Зачем этот скрипт, а не копирование руками:

1. У Zed жёсткий бюджет каталога — 50 КБ на сумму `name` + `description` всех
   навыков. Полный набор ECC (325 навыков) весит ~111 КБ, то есть 217% бюджета.
   Навыки, которые не влезают, Zed молча выбрасывает из каталога с warning'ом
   в UI. Скрипт считает бюджет сам и тем, кто не влез, проставляет
   `disable-model-invocation: true`: они остаются доступны руками
   (`/имя-навыка` в редакторе сообщений и @-упоминание), но не занимают
   место в авто-каталоге.

2. Глобальная папка общая для всех проектов. Скрипт ведёт манифест
   (`~/.agents/.ecc-skills-manifest.json`) и трогает ТОЛЬКО те навыки, которые
   поставил сам. Чужие навыки в той же папке не перезаписываются.

Использование:
    python3 tools/install_zed_skills.py --source .agents/skills
    python3 tools/install_zed_skills.py --source .agents/skills --dry-run
    python3 tools/install_zed_skills.py --budget 51200 --priority go,tdd,test
"""

from __future__ import annotations

import argparse
import json
import os
import re
import shutil
import sys
from pathlib import Path

MANIFEST_NAME = ".ecc-skills-manifest.json"
ZED_BUDGET_DEFAULT = 51200  # 50 КБ, документировано в zed.dev/docs/ai/skills

# Поля frontmatter, которые Zed не понимает, но которые безвредны для него.
# Оставляем как есть: SKILL.md — общий формат (agentskills.io), лишние поля
# Zed игнорирует, а они нужны другим harness'ам (Codex, Cursor).
FRONTMATTER_RE = re.compile(r"^---\n(.*?)\n---", re.S)


def parse_frontmatter(text: str) -> tuple[list[str], str]:
    """Возвращает (строки frontmatter, тело). Без frontmatter — пустой список."""
    m = FRONTMATTER_RE.match(text)
    if not m:
        return [], text
    return m.group(1).splitlines(), text[m.end():]


def read_skill(skill_md: Path) -> dict:
    """Достаёт name/description и сам disable-model-invocation."""
    text = skill_md.read_text(encoding="utf-8", errors="replace")
    lines, _ = parse_frontmatter(text)

    name = None
    desc = ""
    disabled = False
    i = 0
    while i < len(lines):
        line = lines[i]
        if line.startswith("name:"):
            name = line.split(":", 1)[1].strip().strip("\"'")
        elif line.startswith("description:"):
            desc = line.split(":", 1)[1].strip()
            # Свёрнутое описание (| или >) продолжается следующими строками,
            # пока не начнётся новое поле.
            if desc in ("|", ">", "|-", ">-", "|+", ">+"):
                buf = []
                for nxt in lines[i + 1:]:
                    if re.match(r"^[A-Za-z_-]+:", nxt):
                        break
                    buf.append(nxt.strip())
                desc = " ".join(b for b in buf if b)
        elif line.strip() == "disable-model-invocation: true":
            disabled = True
        i += 1

    return {"name": name, "description": desc, "disabled": disabled, "body_lines": len(lines)}


def is_budget_safe_name(name: str) -> bool:
    """Имя, которое Zed вообще примет (иначе навык не загрузится)."""
    return bool(re.fullmatch(r"[a-z0-9]+(-[a-z0-9]+)*", name or "")) and len(name or "") <= 64


def catalog_size(name: str, desc: str) -> int:
    """Сколько байт навык съедает из 50-КБ бюджета каталога."""
    return len(name) + len(desc) + 2


def pick_auto_catalog(entries: list[dict], budget: int, priority: list[str]) -> set[str]:
    """Жадно набирает навыки в бюджет, отдавая предпочтение релевантным.

    Сортировка стабильная: сначала навыки, в имени которых встречается
    хотя бы один маркер из --priority, затем остальные по алфавиту.
    """
    def rank(e: dict) -> tuple[int, str]:
        hits = sum(1 for p in priority if p in e["name"])
        return (-hits, e["name"])

    chosen: set[str] = set()
    used = 0
    for e in sorted(entries, key=rank):
        size = e["size"]
        if used + size <= budget:
            chosen.add(e["name"])
            used += size
    return chosen


def strip_disable_flag(lines: list[str]) -> list[str]:
    return [l for l in lines if l.strip() != "disable-model-invocation: true"]


def write_skill_md(skill_md: Path, lines: list[str], body: str, disable: bool) -> None:
    """Пересобирает SKILL.md с корректным флагом авто-вызова."""
    out = list(lines)
    if disable and not any(l.strip() == "disable-model-invocation: true" for l in out):
        out.append("disable-model-invocation: true")
    elif not disable:
        out = strip_disable_flag(out)
    skill_md.write_text("---\n" + "\n".join(out) + "\n---" + body, encoding="utf-8")


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--source", required=True, help="папка с навыками-исходниками")
    ap.add_argument("--dest", default=str(Path.home() / ".agents" / "skills"),
                    help="глобальная папка навыков Zed (по умолчанию ~/.agents/skills)")
    ap.add_argument("--budget", type=int, default=ZED_BUDGET_DEFAULT)
    ap.add_argument("--priority", default="go,security,tdd,test,review,docker,postgres,search,api,auth,ci,refactor,debug,architect,frontend,docs",
                    help="маркеры релевантности: их навыки первыми попадают в авто-каталог")
    ap.add_argument("--dry-run", action="store_true")
    args = ap.parse_args()

    src = Path(args.source).resolve()
    dest = Path(args.dest).resolve()
    manifest_path = dest.parent / MANIFEST_NAME

    if not src.is_dir():
        print(f"источник не найден: {src}", file=sys.stderr)
        return 1

    manifest = {}
    if manifest_path.exists():
        try:
            manifest = json.loads(manifest_path.read_text(encoding="utf-8")).get("skills", {})
        except json.JSONDecodeError:
            manifest = {}

    priority = [p.strip() for p in args.priority.split(",") if p.strip()]

    # 1. Собираем навыки-исходники и отсеиваем те, что Zed не примет.
    entries, skipped = [], []
    for d in sorted(p for p in src.iterdir() if p.is_dir()):
        md = d / "SKILL.md"
        if not md.exists():
            skipped.append((d.name, "нет SKILL.md"))
            continue
        meta = read_skill(md)
        name = meta["name"] or d.name
        if not is_budget_safe_name(name):
            skipped.append((d.name, f"недопустимое имя {name!r}"))
            continue
        if len(meta["description"]) > 1024:
            skipped.append((d.name, f"description >1024 симв. ({len(meta['description'])})"))
            continue
        entries.append({"dir": d, "name": name, "md": md,
                        "description": meta["description"],
                        "size": catalog_size(name, meta["description"])})

    if skipped:
        print("пропущено:")
        for n, why in skipped:
            print(f"  - {n}: {why}")

    # 2. Решаем, кто попадает в авто-каталог, а кто остаётся ручным.
    #
    # В папке назначения могут уже лежать навыки, которых нет в источнике и
    # которых нет в манифесте (поставлены вручную). Они тоже занимают бюджет
    # каталога, поэтому вычитаем их ДО набора — иначе итоговый каталог
    # молча переполнится и Zed выбросит часть навыков с warning'ом.
    foreign_names = set(os.listdir(dest)) if dest.is_dir() else set()
    managed = set(manifest)
    source_names = {e["name"] for e in entries}
    preexisting_foreign = sorted(foreign_names - source_names - managed)

    reserved = 0
    if preexisting_foreign:
        for n in preexisting_foreign:
            md = dest / n / "SKILL.md"
            if not md.exists():
                continue
            m = read_skill(md)
            if "disable-model-invocation: true" in \
                    (FRONTMATTER_RE.match(md.read_text(encoding="utf-8", errors="replace")).group(1)):
                continue
            reserved += catalog_size(n, m["description"])

    effective_budget = args.budget - reserved
    if reserved:
        print(f"занято уже установленными навыками: {reserved} Б "
              f"(осталось {effective_budget} Б)")
    if effective_budget <= 0:
        print("бюджет исчерпан чужими навыками — ставим всё ручным", file=sys.stderr)
        effective_budget = 0

    auto = pick_auto_catalog(entries, effective_budget, priority)
    total = sum(e["size"] for e in entries) + reserved
    print(f"\nнавыков: {len(entries)}  каталог: {total} Б  бюджет: {args.budget} Б "
          f"({round(total / args.budget * 100)}%)")
    print(f"авто-каталог: {len(auto)}  ручной вызов (/имя): {len(entries) - len(auto)}")

    # 3. Пишем. Чужие навыки не трогаем.
    dest.mkdir(parents=True, exist_ok=True)
    installed, manual, skipped_foreign, updated = [], [], [], []

    for e in entries:
        target = dest / e["name"]
        is_manual = e["name"] not in auto

        # Навык в папке, которого нет в манифесте, поставили не мы.
        # Чужое не перезаписываем — предупреждаем и идём дальше.
        if target.exists() and e["name"] not in manifest:
            skipped_foreign.append(e["name"])
            continue

        if is_manual:
            manual.append(e["name"])

        if args.dry_run:
            if not target.exists():
                installed.append(e["name"])
            else:
                updated.append(e["name"])
            continue

        existed = target.exists()
        if not existed:
            shutil.copytree(e["dir"], target)
            installed.append(e["name"])
        else:
            updated.append(e["name"])

        # Копия на месте — правим только флаг авто-вызова, тело не трогаем.
        lines, body = parse_frontmatter(e["md"].read_text(encoding="utf-8", errors="replace"))
        write_skill_md(target / "SKILL.md", lines, body, is_manual)

    if skipped_foreign:
        print(f"\nне тронуто (уже были до нас, не наши): {len(skipped_foreign)}")
        for n in sorted(skipped_foreign)[:10]:
            print(f"  ! {n}")
        print("  (переименуй или удали, если это устаревшие копии ECC)")

    if not args.dry_run:
        manifest = {"skills": {e["name"]: {"source": str(e["dir"]), "manual": e["name"] not in auto}
                               for e in entries}}
        manifest_path.write_text(json.dumps(manifest, ensure_ascii=False, indent=1), encoding="utf-8")
        print(f"\nманифест: {manifest_path}")

    print(f"\nустановлено новых: {len(installed)}  обновлено: {len(updated)}")
    if args.dry_run:
        print("(--dry-run: файлы не записывались)")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
