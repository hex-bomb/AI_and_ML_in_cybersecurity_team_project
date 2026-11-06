import os

TOP_K = int(os.environ.get("TOP_K", "4"))
MAX_UPLOAD = int(os.environ.get("MAX_UPLOAD_BYTES", str(5 << 20)))

# Хранилище диалогов. Без него сервис не стартует: история и идемпотентность
# обязательны для контракта ответа.
DATABASE_URL = os.environ.get(
    "ANSWER_DATABASE_URL", "postgresql://vektor:vektor-dev@localhost:5432/vektor_answer"
)

# Генерацию, брошенную дольше этого срока, при старте считаем прерванной:
# иначе диалог остался бы навсегда занятым одной репликой.
TURN_STALE_SECONDS = int(os.environ.get("TURN_STALE_SECONDS", "600"))
