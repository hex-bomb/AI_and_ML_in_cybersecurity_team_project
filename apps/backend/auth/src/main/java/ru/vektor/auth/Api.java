package ru.vektor.auth;

import com.nimbusds.jwt.JWTClaimsSet;
import jakarta.servlet.http.HttpServletRequest;
import java.time.Duration;
import java.util.List;
import java.util.Map;
import org.springframework.beans.factory.annotation.Value;
import org.springframework.http.HttpHeaders;
import org.springframework.http.ResponseCookie;
import org.springframework.http.ResponseEntity;
import org.springframework.web.bind.annotation.CookieValue;
import org.springframework.web.bind.annotation.GetMapping;
import org.springframework.web.bind.annotation.PostMapping;
import org.springframework.web.bind.annotation.RequestBody;
import org.springframework.web.bind.annotation.RestController;

@RestController
public class Api {

    public record LoginRequest(String username, String password) {}

    public record UserView(String id, String name, String role) {}

    private final UserRepository users;
    private final Tokens tokens;
    private final LoginThrottle throttle;
    private final String tenantId;
    private final String kbId;
    private final long ttl;
    private final long adminTtl;
    private final String cookieName;
    private final boolean cookieSecure;
    private final String dummyHash;

    public Api(
        UserRepository users,
        Tokens tokens,
        LoginThrottle throttle,
        @Value("${auth.tenant-id}") String tenantId,
        @Value("${auth.kb-id}") String kbId,
        @Value("${auth.session-ttl}") long ttl,
        @Value("${auth.admin-session-ttl:3153600000}") long adminTtl,
        @Value("${auth.cookie-name}") String cookieName,
        @Value("${auth.cookie-secure}") boolean cookieSecure
    ) {
        this.users = users;
        this.tokens = tokens;
        this.throttle = throttle;
        this.tenantId = tenantId;
        this.kbId = kbId;
        this.ttl = ttl;
        this.adminTtl = adminTtl;
        this.cookieName = cookieName;
        this.cookieSecure = cookieSecure;
        this.dummyHash = users.encoder.encode("dummy-password-for-timing");
    }

    @PostMapping("/api/v1/auth/login")
    public ResponseEntity<Object> login(
        @RequestBody LoginRequest body,
        HttpServletRequest request
    ) {
        var rid = rid(request);
        var login =
            body.username() == null
                ? ""
                : body.username().strip().toLowerCase();
        var password = body.password() == null ? "" : body.password();
        var throttleKey = login + "|" + request.getRemoteAddr();

        if (!throttle.allowed(throttleKey)) {
            return error(
                429,
                "TOO_MANY_ATTEMPTS",
                "Слишком много попыток входа, повторите позже",
                rid
            );
        }

        var found = users.find(login);
        if (found.isEmpty()) {
            users.encoder.matches(password, dummyHash);
            throttle.recordFailure(throttleKey);
            return error(401, "UNAUTHORIZED", "Неверный логин или пароль", rid);
        }

        var user = found.get();
        if (!users.encoder.matches(password, user.passwordHash())) {
            throttle.recordFailure(throttleKey);
            return error(401, "UNAUTHORIZED", "Неверный логин или пароль", rid);
        }
        throttle.reset(throttleKey);

        // admin — сервисный аккаунт: получает adminTtl, то есть почти бессрочный токен.
        var effectiveTtl = "admin".equals(user.role()) ? adminTtl : ttl;

        String token;
        try {
            token = tokens.issue(
                user.id().toString(),
                user.name(),
                user.role(),
                scopes(user.role()),
                tenantId,
                kbId,
                effectiveTtl
            );
        } catch (Exception exception) {
            return error(500, "INTERNAL", "Внутренняя ошибка", rid);
        }

        var cookie = ResponseCookie.from(cookieName, token)
            .httpOnly(true)
            .secure(cookieSecure)
            .sameSite("Lax")
            .path("/")
            .maxAge(Duration.ofSeconds(effectiveTtl))
            .build();

        return ResponseEntity.ok()
            .header(HttpHeaders.SET_COOKIE, cookie.toString())
            .body(
                Map.of(
                    "user",
                    new UserView(
                        user.id().toString(),
                        user.name(),
                        user.role()
                    ),
                    "request_id",
                    rid
                )
            );
    }

    @PostMapping("/api/v1/auth/logout")
    public ResponseEntity<Object> logout() {
        var cookie = ResponseCookie.from(cookieName, "")
            .httpOnly(true)
            .secure(cookieSecure)
            .sameSite("Lax")
            .path("/")
            .maxAge(0)
            .build();
        return ResponseEntity.noContent()
            .header(HttpHeaders.SET_COOKIE, cookie.toString())
            .build();
    }

    @GetMapping("/api/v1/auth/me")
    public ResponseEntity<Object> me(
        @CookieValue(
            name = "${auth.cookie-name}",
            required = false
        ) String token,
        HttpServletRequest request
    ) {
        var rid = rid(request);
        if (token == null || token.isBlank()) {
            return error(401, "UNAUTHORIZED", "Требуется авторизация", rid);
        }
        try {
            JWTClaimsSet claims = tokens.verify(token);
            return ResponseEntity.ok(
                Map.of(
                    "user",
                    new UserView(
                        claims.getSubject(),
                        String.valueOf(claims.getClaim("name")),
                        String.valueOf(claims.getClaim("role"))
                    ),
                    "request_id",
                    rid
                )
            );
        } catch (Exception exception) {
            return error(401, "UNAUTHORIZED", "Некорректная сессия", rid);
        }
    }

    @GetMapping(value = "/.well-known/jwks.json", produces = "application/json")
    public String jwks() {
        return tokens.jwks();
    }

    // Единственное место, где роль превращается в аудитории: остальные сервисы
    // доверяют только scopes из токена, а не полю role.
    static List<String> scopes(String role) {
        return switch (role) {
            case "admin" -> List.of("public", "employee");
            case "employee" -> List.of("public", "employee");
            default -> List.of("public");
        };
    }

    private static String rid(HttpServletRequest request) {
        var value = request.getAttribute("rid");
        return value == null ? "" : value.toString();
    }

    private static ResponseEntity<Object> error(
        int status,
        String code,
        String message,
        String rid
    ) {
        return ResponseEntity.status(status).body(
            Map.of(
                "error",
                Map.of("code", code, "message", message),
                "request_id",
                rid
            )
        );
    }
}
