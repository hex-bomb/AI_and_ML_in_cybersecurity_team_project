from typing import Any


class Unavailable(Exception):
    """Соседний сервис недоступен: наружу уйдёт 503 с кодом по месту вызова."""


class BadRequest(Exception):
    """Соседний сервис отверг вход: вина входа, а не конфигурации."""


class AppError(Exception):
    """Ошибка сценария, готовая к переводу в HTTP-ответ."""

    def __init__(
        self,
        status: int,
        code: str,
        message: str,
        details: dict[str, Any] | None = None,
    ) -> None:
        super().__init__(code)
        self.status = status
        self.code = code
        self.message = message
        self.details = details

    # Конструкторы: единственное место, где живут коды и тексты.
    @staticmethod
    def unauthorized(message: str = "Требуется авторизация") -> "AppError":
        return AppError(401, "UNAUTHORIZED", message)

    @staticmethod
    def auth_unavailable() -> "AppError":
        return AppError(503, "AUTH_UNAVAILABLE", "Сервис авторизации недоступен")

    @staticmethod
    def kb_not_found() -> "AppError":
        return AppError(404, "KB_NOT_FOUND", "Неизвестная база знаний")

    @staticmethod
    def forbidden(
        message: str = "Доступ запрещён",
        details: dict[str, Any] | None = None,
    ) -> "AppError":
        return AppError(403, "FORBIDDEN", message, details)

    @staticmethod
    def bad_request(message: str) -> "AppError":
        return AppError(400, "INVALID_REQUEST", message)

    @staticmethod
    def too_large() -> "AppError":
        return AppError(413, "PAYLOAD_TOO_LARGE", "Файл больше лимита")

    @staticmethod
    def search_unavailable() -> "AppError":
        return AppError(503, "SEARCH_UNAVAILABLE", "Поиск недоступен")

    @staticmethod
    def llm_unavailable() -> "AppError":
        return AppError(503, "LLM_UNAVAILABLE", "Генерация ответа недоступна")

    @staticmethod
    def service_unavailable() -> "AppError":
        return AppError(503, "SERVICE_UNAVAILABLE", "Сервис недоступен")

    @staticmethod
    def bad_output() -> "AppError":
        return AppError(502, "LLM_BAD_OUTPUT", "Модель вернула пустой ответ")

    @staticmethod
    def internal() -> "AppError":
        return AppError(500, "INTERNAL", "Внутренняя ошибка")
