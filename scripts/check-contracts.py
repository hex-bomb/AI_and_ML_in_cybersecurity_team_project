"""Проверяет согласованность OpenAPI, Postman и router-конфига."""

import json
import re
import sys
from pathlib import Path

import yaml

ROOT = Path(__file__).resolve().parents[1]
SPEC = ROOT / "contracts" / "openapi" / "openapi.yaml"
POSTMAN = ROOT / "contracts" / "postman" / "vektor-platform.postman_collection.json"
ROUTER = ROOT / "infra" / "nginx" / "router.conf"

PARAM = re.compile(r"\{[^}]+\}")
POSTMAN_PARAM = re.compile(r"\{\{[^}]+\}\}")
LOCATION = re.compile(r"location\s+(?:(=|\^~)\s+)?(\S+)\s*\{")


def normalize(path: str) -> str:
    return PARAM.sub("{param}", POSTMAN_PARAM.sub("{param}", path))


def walk(node, visitor):
    if isinstance(node, dict):
        visitor(node)
        for value in node.values():
            walk(value, visitor)
    elif isinstance(node, list):
        for value in node:
            walk(value, visitor)


def check_refs(spec: dict) -> list[str]:
    problems: list[str] = []

    def resolve(ref: str):
        node = spec
        for part in ref.lstrip("#/").split("/"):
            node = node.get(part) if isinstance(node, dict) else None
            if node is None:
                return None
        return node

    def visitor(node: dict) -> None:
        ref = node.get("$ref")
        if isinstance(ref, str) and ref.startswith("#/") and resolve(ref) is None:
            problems.append(f"битая ссылка {ref}")

    walk(spec, visitor)
    return problems


def check_operations(spec: dict) -> list[str]:
    problems: list[str] = []
    for path, item in spec.get("paths", {}).items():
        methods = {key for key in item if key in {"get", "post", "put", "patch", "delete"}}
        if not methods:
            problems.append(f"{path} без операций")
    return problems


def postman_paths() -> list[str]:
    collection = json.loads(POSTMAN.read_text(encoding="utf-8"))
    out: list[str] = []

    def visit(items) -> None:
        for item in items:
            if "item" in item:
                visit(item["item"])
                continue
            url = item.get("request", {}).get("url")
            if isinstance(url, str):
                out.append(re.sub(r"^\{\{[^}]+\}\}", "", url))

    visit(collection.get("item", []))
    return out


def router_paths() -> list[tuple[str, str]]:
    """`location = x` → Exact, `location ^~ x` и `location /` → Prefix."""
    out: list[tuple[str, str]] = []
    for line in ROUTER.read_text(encoding="utf-8").splitlines():
        if line.lstrip().startswith("#"):
            continue
        match = LOCATION.search(line)
        if match:
            modifier, path = match.group(1), match.group(2)
            out.append(("Exact" if modifier == "=" else "Prefix", path))
    return out


def covered(path: str, routes: list[tuple[str, str]]) -> bool:
    for path_type, prefix in routes:
        if path_type == "Exact" and path == prefix:
            return True
        if path_type == "Prefix" and path.startswith(prefix):
            return True
    return False


def main() -> int:
    spec = yaml.safe_load(SPEC.read_text(encoding="utf-8"))
    problems = check_refs(spec) + check_operations(spec)

    spec_paths = {normalize(path) for path in spec.get("paths", {})}
    for path in postman_paths():
        if normalize(path) not in spec_paths:
            problems.append(f"postman: {path} нет в OpenAPI")

    routes = router_paths()
    for path in spec_paths:
        if not covered(path, routes):
            problems.append(f"router: {path} не маршрутизируется")

    for problem in problems:
        print(problem)
    print(
        f"openapi: {len(spec_paths)} путей, postman: {len(postman_paths())} запросов, "
        f"router: {len(routes)} правил"
    )
    return 1 if problems else 0


if __name__ == "__main__":
    sys.exit(main())
