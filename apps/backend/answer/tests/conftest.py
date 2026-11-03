import os
import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))

# Настройки читаются при импорте app.deps (там создаётся Clients).
# В тестах сеть не нужна, но URL обязан быть валидным.
os.environ.setdefault("JWKS_URL", "http://auth.test/.well-known/jwks.json")
