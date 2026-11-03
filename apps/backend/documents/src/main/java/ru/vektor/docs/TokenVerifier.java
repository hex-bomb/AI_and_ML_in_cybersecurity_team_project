package ru.vektor.docs;

import com.nimbusds.jose.crypto.RSASSAVerifier;
import com.nimbusds.jose.jwk.JWK;
import com.nimbusds.jose.jwk.JWKSet;
import com.nimbusds.jwt.JWTClaimsSet;
import com.nimbusds.jwt.SignedJWT;
import java.net.URI;
import java.net.http.HttpClient;
import java.net.http.HttpRequest;
import java.net.http.HttpResponse;
import java.time.Duration;
import java.time.Instant;
import java.util.Date;
import java.util.List;
import org.slf4j.Logger;
import org.slf4j.LoggerFactory;
import org.springframework.beans.factory.annotation.Value;
import org.springframework.stereotype.Component;

/**
 * Проверяет JWT по JWKS auth-сервиса.
 *
 * <p>Ключи кешируются на {@code ttl} и обновляются под блокировкой, так что
 * одновременные запросы не устраивают шторм к auth. При сбое обновления
 * отдаём ключ из старого кеша, а между неудачными попытками держим паузу
 * {@code RETRY_BACKOFF}. Экземпляр потокобезопасен.
 */
@Component
public class TokenVerifier {

    private static final Logger log = LoggerFactory.getLogger(
        TokenVerifier.class
    );

    private static final Duration TTL = Duration.ofMinutes(5);
    // Пауза после неудачного обновления: иначе недоступный auth заставляет
    // каждый запрос ждать таймаут fetch().
    private static final Duration RETRY_BACKOFF = Duration.ofSeconds(10);

    public record Claims(
        String sub,
        String role,
        List<String> scopes,
        String tenant,
        String kb
    ) {
        /**
         * Право на внутренние документы определяется аудиторией из токена,
         * а не ролью: auth уже разложил роль в scopes, и admin тоже получает
         * "employee". Проверка роли напрямую отсекала сервисный аккаунт.
         */
        public boolean employee() {
            return scopes.contains("employee");
        }

        public boolean canRead(String audience) {
            return scopes.contains(audience);
        }
    }

    private final String jwksUrl;
    private final String issuer;
    private final HttpClient http;
    private final Duration ttl;
    private final Duration backoff;

    private final Object refreshLock = new Object();
    private volatile JWKSet keys;
    private volatile Instant fetchedAt = Instant.EPOCH;
    private volatile Instant nextRefreshAt = Instant.EPOCH;

    public TokenVerifier(
        @Value("${documents.jwks-url}") String jwksUrl,
        @Value("${documents.issuer:vektor-auth}") String issuer
    ) {
        this(
            jwksUrl,
            issuer,
            HttpClient.newBuilder()
                .connectTimeout(Duration.ofSeconds(3))
                .build(),
            TTL,
            RETRY_BACKOFF
        );
    }

    TokenVerifier(
        String jwksUrl,
        String issuer,
        HttpClient http,
        Duration ttl,
        Duration backoff
    ) {
        this.jwksUrl = jwksUrl;
        this.issuer = issuer;
        this.http = http;
        this.ttl = ttl;
        this.backoff = backoff;
    }

    /**
     * Проверяет подпись, срок и issuer и достаёт из токена scopes, tenant и kb.
     *
     * @throws Errors.AppError если токен пустой, битый, просрочен или чужой
     */
    public Claims verify(String token) {
        if (token == null || token.isBlank()) {
            throw Errors.forbidden();
        }
        try {
            var jwt = SignedJWT.parse(token);
            var jwk = key(jwt.getHeader().getKeyID());
            if (!jwt.verify(new RSASSAVerifier(jwk.toRSAKey()))) {
                throw Errors.forbidden();
            }
            var claims = jwt.getJWTClaimsSet();
            var expiresAt = claims.getExpirationTime();
            if (expiresAt == null || expiresAt.before(new Date())) {
                throw Errors.forbidden();
            }
            if (!issuer.isBlank() && !issuer.equals(claims.getIssuer())) {
                throw Errors.forbidden();
            }
            return new Claims(
                claims.getSubject(),
                text(claims, "role"),
                scopes(claims),
                text(claims, "tenant"),
                text(claims, "kb")
            );
        } catch (Errors.AppError error) {
            throw error;
        } catch (Exception exception) {
            // Не логируем на уровне info: мусорные токены — обычный шум.
            log.debug("token rejected", exception);
            throw Errors.forbidden();
        }
    }

    private JWK key(String kid) throws Exception {
        if (fresh()) {
            var found = lookup(kid);
            if (found != null) {
                return found;
            }
        }
        synchronized (refreshLock) {
            if (fresh()) {
                var found = lookup(kid);
                if (found != null) {
                    return found;
                }
            }
            var stale = lookup(kid);
            if (Instant.now().isBefore(nextRefreshAt)) {
                if (stale != null) {
                    return stale;
                }
                throw Errors.forbidden();
            }
            try {
                var refreshed = fetch();
                keys = refreshed;
                fetchedAt = Instant.now();
                nextRefreshAt = Instant.EPOCH;
                var updated = refreshed.getKeyByKeyId(kid);
                if (updated == null) {
                    throw Errors.forbidden();
                }
                return updated;
            } catch (Errors.AppError error) {
                throw error;
            } catch (Exception exception) {
                nextRefreshAt = Instant.now().plus(backoff);
                if (stale != null) {
                    log.warn("jwks refresh failed, используем кэш", exception);
                    return stale;
                }
                throw Errors.forbidden();
            }
        }
    }

    private boolean fresh() {
        return keys != null && Instant.now().isBefore(fetchedAt.plus(ttl));
    }

    private JWK lookup(String kid) {
        var current = keys;
        return current == null ? null : current.getKeyByKeyId(kid);
    }

    private JWKSet fetch() throws Exception {
        var request = HttpRequest.newBuilder(URI.create(jwksUrl))
            .timeout(Duration.ofSeconds(5))
            .GET()
            .build();
        var response = http.send(request, HttpResponse.BodyHandlers.ofString());
        if (response.statusCode() != 200) {
            throw new IllegalStateException(
                "jwks status " + response.statusCode()
            );
        }
        return JWKSet.parse(response.body());
    }

    private static String text(JWTClaimsSet claims, String name) {
        var value = claims.getClaim(name);
        return value == null ? "" : value.toString();
    }

    private static List<String> scopes(JWTClaimsSet claims) {
        var value = claims.getClaim("scp");
        if (value instanceof List<?> list) {
            return list.stream().map(String::valueOf).toList();
        }
        return List.of();
    }
}
