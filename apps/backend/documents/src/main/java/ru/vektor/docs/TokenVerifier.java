package ru.vektor.docs;

import java.net.URI;
import java.net.http.HttpClient;
import java.net.http.HttpRequest;
import java.net.http.HttpResponse;
import java.time.Duration;
import java.time.Instant;
import java.util.Date;
import java.util.List;

import com.nimbusds.jose.crypto.RSASSAVerifier;
import com.nimbusds.jose.jwk.JWK;
import com.nimbusds.jose.jwk.JWKSet;
import com.nimbusds.jwt.JWTClaimsSet;
import com.nimbusds.jwt.SignedJWT;
import org.springframework.beans.factory.annotation.Value;
import org.springframework.stereotype.Component;

@Component
public class TokenVerifier {

    public record Claims(String sub, String role, List<String> scopes, String tenant, String kb) {
        public boolean employee() {
            return "employee".equals(role);
        }

        public boolean canRead(String audience) {
            return scopes.contains(audience);
        }
    }

    private static final Duration TTL = Duration.ofMinutes(5);

    private final String jwksUrl;
    private final HttpClient http = HttpClient.newBuilder()
            .connectTimeout(Duration.ofSeconds(3))
            .build();

    private volatile JWKSet keys;
    private volatile Instant fetchedAt = Instant.EPOCH;

    public TokenVerifier(@Value("${documents.jwks-url}") String jwksUrl) {
        this.jwksUrl = jwksUrl;
    }

    public Claims verify(String token) {
        if (token == null || token.isBlank()) {
            throw Errors.forbidden();
        }
        try {
            var jwt = SignedJWT.parse(token);
            var kid = jwt.getHeader().getKeyID();
            var jwk = key(kid);
            if (!jwt.verify(new RSASSAVerifier(jwk.toRSAKey()))) {
                throw Errors.forbidden();
            }
            var claims = jwt.getJWTClaimsSet();
            var expiresAt = claims.getExpirationTime();
            if (expiresAt == null || expiresAt.before(new Date())) {
                throw Errors.forbidden();
            }
            return new Claims(claims.getSubject(), text(claims, "role"), scopes(claims),
                    text(claims, "tenant"), text(claims, "kb"));
        } catch (Errors.AppError error) {
            throw error;
        } catch (Exception exception) {
            throw Errors.forbidden();
        }
    }

    private JWK key(String kid) throws Exception {
        var found = keys().getKeyByKeyId(kid);
        if (found != null) {
            return found;
        }
        fetchedAt = Instant.EPOCH;
        found = keys().getKeyByKeyId(kid);
        if (found == null) {
            throw Errors.forbidden();
        }
        return found;
    }

    private JWKSet keys() throws Exception {
        if (keys == null || Instant.now().isAfter(fetchedAt.plus(TTL))) {
            var request = HttpRequest.newBuilder(URI.create(jwksUrl))
                    .timeout(Duration.ofSeconds(5))
                    .GET()
                    .build();
            var response = http.send(request, HttpResponse.BodyHandlers.ofString());
            if (response.statusCode() != 200) {
                throw Errors.forbidden();
            }
            keys = JWKSet.parse(response.body());
            fetchedAt = Instant.now();
        }
        return keys;
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
