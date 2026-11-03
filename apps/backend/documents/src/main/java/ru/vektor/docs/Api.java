package ru.vektor.docs;

import jakarta.servlet.http.HttpServletRequest;
import java.nio.charset.StandardCharsets;
import java.security.MessageDigest;
import java.util.HexFormat;
import java.util.Locale;
import java.util.Map;
import java.util.Objects;
import java.util.UUID;
import org.springframework.beans.factory.annotation.Value;
import org.springframework.http.MediaType;
import org.springframework.http.ResponseEntity;
import org.springframework.web.bind.annotation.CookieValue;
import org.springframework.web.bind.annotation.GetMapping;
import org.springframework.web.bind.annotation.PathVariable;
import org.springframework.web.bind.annotation.PostMapping;
import org.springframework.web.bind.annotation.RequestBody;
import org.springframework.web.bind.annotation.RequestHeader;
import org.springframework.web.bind.annotation.RequestParam;
import org.springframework.web.bind.annotation.RequestPart;
import org.springframework.web.bind.annotation.RestController;
import org.springframework.web.multipart.MultipartFile;

@RestController
public class Api {

    private static final String INTERNAL_TOKEN_HEADER = "X-Internal-Token";
    // Список документов отдаётся порциями: без предела один большой корпус
    // гарантирует тяжёлый ответ и лишнюю память на каждом запросе.
    private static final int DEFAULT_LIST_LIMIT = 100;
    private static final int MAX_LIST_LIMIT = 500;
    // Ниже порога документ считается нераспознанным: скан без текстового слоя
    // индексировать нельзя, иначе поиск молча получит пустой документ.
    private static final int MIN_TEXT_CHARS = 20;

    public record IngestRequest(
        String externalId,
        String title,
        String channel,
        String section,
        String text,
        String versionLabel
    ) {}

    public record PublishRequest(UUID versionId) {}

    public record IndexReport(
        int generation,
        String embeddingModel,
        int embeddingDim,
        int chunkCount
    ) {}

    private final DocumentRepository documents;
    private final Extract extract;
    private final TokenVerifier auth;
    private final BlobStore blobs;
    private final String internalToken;
    private final long maxBytes;
    private final int maxTextChars;

    public Api(
        DocumentRepository documents,
        Extract extract,
        TokenVerifier auth,
        BlobStore blobs,
        @Value("${documents.internal-token:}") String internalToken,
        @Value("${documents.max-upload-bytes}") long maxBytes,
        @Value("${documents.max-text-chars:2000000}") int maxTextChars
    ) {
        this.documents = documents;
        this.extract = extract;
        this.auth = auth;
        this.blobs = blobs;
        this.internalToken = internalToken;
        this.maxBytes = maxBytes;
        this.maxTextChars = maxTextChars;
    }

    @PostMapping(
        value = "/api/v1/documents",
        consumes = MediaType.MULTIPART_FORM_DATA_VALUE
    )
    public ResponseEntity<Object> upload(
        @RequestPart("file") MultipartFile file,
        @RequestParam(value = "title", required = false) String title,
        @RequestParam(
            value = "channel",
            required = false,
            defaultValue = "public"
        ) String channel,
        @CookieValue(
            name = "${auth.cookie-name:vektor_session}",
            required = false
        ) String token,
        HttpServletRequest request
    ) throws Exception {
        var claims = auth.verify(token);
        if (!claims.employee()) {
            throw Errors.forbidden();
        }
        if (file == null || file.isEmpty()) {
            throw Errors.badRequest("нужен файл");
        }
        var content = file.getBytes();
        if (content.length > maxBytes) {
            throw Errors.tooLarge();
        }

        var text = extract.text(file.getOriginalFilename(), content);
        // Ключ документа — от содержимого: разные файлы с похожими именами
        // не перетирают друг друга, а повторная загрузка того же файла
        // возвращает существующую версию.
        var externalId = externalId(content);
        var name =
            title != null && !title.isBlank()
                ? title.strip()
                : stem(file.getOriginalFilename());

        var version = store(
            externalId,
            clip(name, 300),
            "Загружено",
            channel(channel),
            content,
            file.getContentType(),
            text,
            "upload"
        );

        return ResponseEntity.status(201).body(
            Map.of(
                "document_id",
                externalId,
                "version_id",
                version.id().toString(),
                "created",
                version.created(),
                "request_id",
                rid(request)
            )
        );
    }

    @GetMapping("/api/v1/documents")
    public ResponseEntity<Object> list(
        @CookieValue(
            name = "${auth.cookie-name:vektor_session}",
            required = false
        ) String token,
        @RequestParam(
            value = "limit",
            required = false,
            defaultValue = "" + DEFAULT_LIST_LIMIT
        ) int limit,
        HttpServletRequest request
    ) {
        var claims = auth.verify(token);
        if (!claims.employee()) {
            throw Errors.forbidden();
        }
        var bounded = Math.clamp(limit, 1, MAX_LIST_LIMIT);
        return ResponseEntity.ok(
            Map.of(
                "documents",
                documents.list(claims.channels(), claims.groups(), bounded),
                "request_id",
                rid(request)
            )
        );
    }

    @GetMapping("/api/v1/documents/{externalId}/source")
    public ResponseEntity<String> source(
        @PathVariable String externalId,
        @CookieValue(
            name = "${auth.cookie-name:vektor_session}",
            required = false
        ) String token
    ) throws Exception {
        var claims = auth.verify(token);
        var found = documents.source(
            externalId,
            claims.channels(),
            claims.groups()
        );
        // Нет прав отдаём как «нет документа»: иначе 403 раскрывает,
        // что такой документ существует.
        if (found.isEmpty()) {
            throw Errors.notFound();
        }
        var source = found.get();
        var key = documents
            .storageKey(source.textBlobId())
            .orElseThrow(Errors::notFound);
        return ResponseEntity.ok()
            .contentType(new MediaType("text", "plain", StandardCharsets.UTF_8))
            .body(new String(blobs.read(key), StandardCharsets.UTF_8));
    }

    @PostMapping("/api/v1/documents/{externalId}/publish")
    public ResponseEntity<Object> publish(
        @PathVariable String externalId,
        @RequestBody(required = false) PublishRequest body,
        @CookieValue(
            name = "${auth.cookie-name:vektor_session}",
            required = false
        ) String token,
        HttpServletRequest request
    ) {
        var claims = auth.verify(token);
        if (!claims.employee()) {
            throw Errors.forbidden();
        }
        var publication = documents.publish(
            externalId,
            body == null ? null : body.versionId()
        );
        return ResponseEntity.ok(
            Map.of(
                "document_id",
                publication.externalId(),
                "version_id",
                publication.versionId().toString(),
                "publication",
                "published",
                "channels",
                publication.channels(),
                "groups",
                publication.groups(),
                "request_id",
                rid(request)
            )
        );
    }

    @PostMapping("/api/v1/documents/{externalId}/withdraw")
    public ResponseEntity<Object> withdraw(
        @PathVariable String externalId,
        @CookieValue(
            name = "${auth.cookie-name:vektor_session}",
            required = false
        ) String token,
        HttpServletRequest request
    ) {
        var claims = auth.verify(token);
        if (!claims.employee()) {
            throw Errors.forbidden();
        }
        documents.withdraw(externalId);
        return ResponseEntity.ok(
            Map.of(
                "document_id",
                externalId,
                "publication",
                "withdrawn",
                "request_id",
                rid(request)
            )
        );
    }

    /** Публикация для заливки корпуса: у скрипта нет cookie, только внутренний токен. */
    @PostMapping("/internal/publish/{externalId}")
    public ResponseEntity<Object> publishInternal(
        @PathVariable String externalId,
        @RequestHeader(
            name = INTERNAL_TOKEN_HEADER,
            required = false
        ) String token
    ) {
        checkInternal(token);
        var publication = documents.publish(externalId, null);
        return ResponseEntity.ok(
            Map.of(
                "document_id",
                publication.externalId(),
                "version_id",
                publication.versionId().toString()
            )
        );
    }

    @PostMapping("/internal/documents")
    public ResponseEntity<Object> ingest(@RequestBody IngestRequest body)
        throws Exception {
        if (
            body.externalId() == null ||
            body.externalId().isBlank() ||
            body.text() == null
        ) {
            throw Errors.badRequest("нужны external_id и text");
        }
        if (body.text().length() > maxTextChars) {
            throw Errors.tooLarge();
        }
        var text = body.text().strip();
        if (text.length() < MIN_TEXT_CHARS) {
            throw Errors.badRequest("в документе нет извлекаемого текста");
        }
        var section = body.section() == null ? "Прочее" : body.section();
        var title = clip(
            body.title() == null ? body.externalId() : body.title(),
            300
        );

        var version = store(
            body.externalId(),
            title,
            section,
            channel(body.channel()),
            null,
            null,
            text,
            body.versionLabel() == null ? "v1" : body.versionLabel()
        );

        return ResponseEntity.ok(
            Map.of(
                "document_id",
                body.externalId(),
                "version_id",
                version.id().toString(),
                "created",
                version.created()
            )
        );
    }

    /** Отчёт индексатора: версия становится ready только для текущей генерации. */
    @PostMapping("/internal/versions/{versionId}/index")
    public ResponseEntity<Object> indexed(
        @PathVariable UUID versionId,
        @RequestBody IndexReport report,
        @RequestHeader(
            name = INTERNAL_TOKEN_HEADER,
            required = false
        ) String token
    ) {
        checkInternal(token);
        if (report.embeddingDim() <= 0 || report.generation() <= 0) {
            throw Errors.badRequest(
                "generation и embedding_dim должны быть > 0"
            );
        }
        var accepted = documents.markIndexed(
            versionId,
            report.generation(),
            report.embeddingModel(),
            report.embeddingDim(),
            report.chunkCount()
        );
        return ResponseEntity.ok(
            Map.of("accepted", accepted, "version_id", versionId.toString())
        );
    }

    @GetMapping(
        value = "/internal/documents/{externalId}/versions/{versionId}/text",
        produces = MediaType.TEXT_PLAIN_VALUE
    )
    public ResponseEntity<String> versionText(
        @PathVariable String externalId,
        @PathVariable UUID versionId,
        @RequestHeader(
            name = INTERNAL_TOKEN_HEADER,
            required = false
        ) String token
    ) throws Exception {
        checkInternal(token);
        var blobId = documents
            .textBlob(externalId, versionId)
            .orElseThrow(Errors::notFound);
        var key = documents.storageKey(blobId).orElseThrow(Errors::notFound);
        return ResponseEntity.ok(
            new String(blobs.read(key), StandardCharsets.UTF_8)
        );
    }

    @PostMapping(
        value = "/internal/extract",
        consumes = MediaType.MULTIPART_FORM_DATA_VALUE
    )
    public ResponseEntity<Object> extractOnly(
        @RequestPart("file") MultipartFile file,
        @RequestHeader(
            name = INTERNAL_TOKEN_HEADER,
            required = false
        ) String token
    ) throws Exception {
        checkInternal(token);
        var content = file.getBytes();
        if (content.length > maxBytes) {
            throw Errors.tooLarge();
        }
        return ResponseEntity.ok(
            Map.of("text", extract.text(file.getOriginalFilename(), content))
        );
    }

    /**
     * Сохраняет оригинал и извлечённый текст, затем создаёт версию.
     *
     * <p>Повторная загрузка того же текста не пишет блобы: версия ищется по
     * хешу заранее, иначе каждый дубль оставлял бы мусор в хранилище.
     */
    private DocumentRepository.Version store(
        String externalId,
        String title,
        String section,
        String channel,
        byte[] original,
        String contentType,
        String text,
        String versionLabel
    ) throws Exception {
        var sha = DocumentRepository.sha256(text);
        var existing = documents.versionId(externalId, sha);
        if (existing.isPresent()) {
            documents.setChannel(externalId, channel);
            return new DocumentRepository.Version(existing.get(), sha, false);
        }

        var textBytes = text.getBytes(StandardCharsets.UTF_8);
        var originalBytes = original == null ? textBytes : original;
        var originalType =
            contentType == null || contentType.isBlank()
                ? "application/octet-stream"
                : contentType;

        var originalKey = blobs.put(originalBytes);
        var textKey = original == null ? originalKey : blobs.put(textBytes);
        var originalBlob = documents.insertBlob(
            originalKey,
            originalBytes.length,
            sha256(originalBytes),
            originalType
        );
        var textBlob =
            original == null
                ? originalBlob
                : documents.insertBlob(
                      textKey,
                      textBytes.length,
                      sha,
                      "text/plain"
                  );

        return documents.upsert(
            externalId,
            title,
            section,
            originalBlob,
            textBlob,
            sha,
            versionLabel,
            "extract",
            "1",
            text.length(),
            text.length() < 200 ? "low_text" : "ok",
            channel
        );
    }

    /**
     * Внутренние эндпоинты закрыты общим токеном: без NetworkPolicy в стенде
     * любой под в кластере мог бы вливать и читать документы.
     */
    private void checkInternal(String token) {
        var expected = internalToken.getBytes(StandardCharsets.UTF_8);
        var actual =
            token == null
                ? new byte[0]
                : token.getBytes(StandardCharsets.UTF_8);
        // Пустой настроенный токен запрещает всё (fail closed), а сравнение
        // не должно зависеть от префикса: иначе токен подбирается по таймингу.
        if (expected.length == 0 || !MessageDigest.isEqual(expected, actual)) {
            throw Errors.forbidden();
        }
    }

    /**
     * Канал — закрытый словарь, он же ограничение в схеме БД. Без проверки
     * невалидное значение падало бы 500 на constraint.
     */
    private static String channel(String raw) {
        if (raw == null || raw.isBlank()) {
            return "public";
        }
        var value = raw.strip().toLowerCase(Locale.ROOT);
        return switch (value) {
            case "public", "internal" -> value;
            default -> throw Errors.badRequest("channel: public или internal");
        };
    }

    private static String rid(HttpServletRequest request) {
        return Objects.toString(request.getAttribute("rid"), "");
    }

    private static String externalId(byte[] content) throws Exception {
        var digest = MessageDigest.getInstance("SHA-256").digest(content);
        // 16 hex-символов — короткий id для URL; полный хеш хранится в версии.
        return HexFormat.of().formatHex(digest).substring(0, 16);
    }

    private static String sha256(byte[] content) throws Exception {
        var digest = MessageDigest.getInstance("SHA-256").digest(content);
        return HexFormat.of().formatHex(digest);
    }

    private static String stem(String filename) {
        var name = filename == null ? "" : filename.replace('\\', '/');
        name = name.substring(name.lastIndexOf('/') + 1);
        var dot = name.lastIndexOf('.');
        return dot <= 0 ? name : name.substring(0, dot);
    }

    private static String clip(String value, int limit) {
        var text = value == null ? "" : value.strip();
        return text.length() <= limit ? text : text.substring(0, limit);
    }
}
