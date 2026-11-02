package ru.vektor.docs;

import jakarta.servlet.http.HttpServletRequest;
import java.nio.charset.StandardCharsets;
import java.security.MessageDigest;
import java.util.HexFormat;
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

    private static final String COOKIE = "vektor_session";
    private static final String INTERNAL_TOKEN_HEADER = "X-Internal-Token";

    public record IngestRequest(
        String kbId,
        String externalId,
        String title,
        String audience,
        String section,
        String text,
        String versionLabel
    ) {}

    private final DocumentRepository documents;
    private final Extract extract;
    private final TokenVerifier auth;
    private final Events events;
    private final String internalToken;
    private final long maxBytes;
    private final String defaultKb;

    public Api(
        DocumentRepository documents,
        Extract extract,
        TokenVerifier auth,
        Events events,
        @Value("${documents.internal-token:}") String internalToken,
        @Value("${documents.max-upload-bytes}") long maxBytes,
        @Value("${documents.default-kb}") String defaultKb
    ) {
        this.documents = documents;
        this.extract = extract;
        this.auth = auth;
        this.events = events;
        this.internalToken = internalToken;
        this.maxBytes = maxBytes;
        this.defaultKb = defaultKb;
    }

    @PostMapping(
        value = "/api/v1/documents",
        consumes = MediaType.MULTIPART_FORM_DATA_VALUE
    )
    public ResponseEntity<Object> upload(
        @RequestPart("file") MultipartFile file,
        @RequestParam(value = "title", required = false) String title,
        @RequestParam(
            value = "audience",
            required = false,
            defaultValue = "public"
        ) String audience,
        @CookieValue(name = COOKIE, required = false) String token,
        HttpServletRequest request
    ) throws Exception {
        var claims = auth.verify(token);
        if (!claims.employee()) {
            throw Errors.forbidden();
        }
        var content = file.getBytes();
        if (content.length > maxBytes) {
            throw Errors.tooLarge();
        }

        var text = extract.text(file.getOriginalFilename(), content);
        // Ключ документа — от содержимого: разные файлы с похожими именами не перетирают друг друга.
        var externalId = externalId(content);
        var name =
            title != null && !title.isBlank()
                ? title.strip()
                : stem(file.getOriginalFilename());
        var version = documents.upsert(
            claims.kb(),
            externalId,
            clip(name, 300),
            audience,
            "Загружено",
            text,
            "upload"
        );

        events.upserted(
            claims.kb(),
            externalId,
            version.id().toString(),
            audience,
            name,
            version.sha256()
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
        @CookieValue(name = COOKIE, required = false) String token,
        HttpServletRequest request
    ) {
        var claims = auth.verify(token);
        if (!claims.employee()) {
            throw Errors.forbidden();
        }
        return ResponseEntity.ok(
            Map.of(
                "documents",
                documents.list(claims.kb()),
                "request_id",
                rid(request)
            )
        );
    }

    @GetMapping("/api/v1/documents/{externalId}/source")
    public ResponseEntity<String> source(
        @PathVariable String externalId,
        @CookieValue(name = COOKIE, required = false) String token
    ) {
        var claims = auth.verify(token);
        var document = documents.latest(claims.kb(), externalId);
        // «Нет документа» и «нет прав» неразличимы снаружи.
        if (document.isEmpty() || !claims.canRead(document.get().audience())) {
            throw Errors.notFound();
        }
        return ResponseEntity.ok()
            .contentType(new MediaType("text", "plain", StandardCharsets.UTF_8))
            .body(document.get().text());
    }

    @PostMapping("/internal/documents")
    public ResponseEntity<Object> ingest(
        @RequestBody IngestRequest body,
        @RequestHeader(
            name = INTERNAL_TOKEN_HEADER,
            required = false
        ) String token
    ) {
        checkInternal(token);
        if (
            body.externalId() == null ||
            body.externalId().isBlank() ||
            body.text() == null
        ) {
            throw Errors.badRequest("нужны external_id и text");
        }
        var kb =
            body.kbId() == null || body.kbId().isBlank()
                ? defaultKb
                : body.kbId();
        var audience = body.audience() == null ? "public" : body.audience();
        var section = body.section() == null ? "Прочее" : body.section();
        var title = clip(
            body.title() == null ? body.externalId() : body.title(),
            300
        );
        var label = body.versionLabel() == null ? "v1" : body.versionLabel();

        var version = documents.upsert(
            kb,
            body.externalId(),
            title,
            audience,
            section,
            body.text(),
            label
        );
        events.upserted(
            kb,
            body.externalId(),
            version.id().toString(),
            audience,
            title,
            version.sha256()
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
    ) {
        checkInternal(token);
        return ResponseEntity.ok(
            documents.text(externalId, versionId).orElseThrow(Errors::notFound)
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
     * Внутренние эндпоинты закрыты общим токеном: без NetworkPolicy в стенде
     * любой под в кластере мог бы вливать и читать документы.
     */
    private void checkInternal(String token) {
        if (internalToken.isBlank() || !internalToken.equals(token)) {
            throw Errors.forbidden();
        }
    }

    private static String rid(HttpServletRequest request) {
        return Objects.toString(request.getAttribute("rid"), "");
    }

    private static String externalId(byte[] content) throws Exception {
        var digest = MessageDigest.getInstance("SHA-256").digest(content);
        return HexFormat.of().formatHex(digest).substring(0, 16);
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
