package ru.vektor.docs;

import java.util.Map;
import java.util.Objects;

import jakarta.servlet.http.HttpServletRequest;
import org.springframework.http.ResponseEntity;
import org.springframework.web.bind.annotation.ExceptionHandler;
import org.springframework.web.bind.annotation.RestControllerAdvice;
import org.springframework.web.multipart.MaxUploadSizeExceededException;

@RestControllerAdvice
public class Errors {

    public static class AppError extends RuntimeException {
        final int status;
        final String code;

        public AppError(int status, String code, String message) {
            super(message);
            this.status = status;
            this.code = code;
        }
    }

    static AppError badRequest(String message) {
        return new AppError(400, "INVALID_REQUEST", message);
    }

    static AppError notFound() {
        return new AppError(404, "NOT_FOUND", "Ресурс не найден");
    }

    static AppError forbidden() {
        return new AppError(403, "FORBIDDEN", "Доступ запрещён");
    }

    static AppError tooLarge() {
        return new AppError(413, "PAYLOAD_TOO_LARGE", "Файл больше лимита");
    }

    @ExceptionHandler(AppError.class)
    ResponseEntity<Object> handle(AppError error, HttpServletRequest request) {
        return body(error.status, error.code, error.getMessage(), request);
    }

    @ExceptionHandler(MaxUploadSizeExceededException.class)
    ResponseEntity<Object> handleTooLarge(HttpServletRequest request) {
        return body(413, "PAYLOAD_TOO_LARGE", "Файл больше лимита", request);
    }

    private static ResponseEntity<Object> body(int status, String code, String message, HttpServletRequest request) {
        return ResponseEntity.status(status).body(Map.of(
                "error", Map.of("code", code, "message", message),
                "request_id", Objects.toString(request.getAttribute("rid"), "")));
    }
}
