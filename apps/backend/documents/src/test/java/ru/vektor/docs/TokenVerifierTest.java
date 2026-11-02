package ru.vektor.docs;

import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertFalse;
import static org.junit.jupiter.api.Assertions.assertThrows;
import static org.junit.jupiter.api.Assertions.assertTrue;

import com.nimbusds.jose.JWSAlgorithm;
import com.nimbusds.jose.JWSHeader;
import com.nimbusds.jose.crypto.RSASSASigner;
import com.nimbusds.jose.jwk.JWKSet;
import com.nimbusds.jose.jwk.RSAKey;
import com.nimbusds.jwt.JWTClaimsSet;
import com.nimbusds.jwt.SignedJWT;
import com.sun.net.httpserver.HttpServer;
import java.net.InetSocketAddress;
import java.net.http.HttpClient;
import java.nio.charset.StandardCharsets;
import java.security.KeyPair;
import java.security.KeyPairGenerator;
import java.security.interfaces.RSAPrivateKey;
import java.time.Duration;
import java.time.Instant;
import java.util.Date;
import java.util.List;
import java.util.concurrent.Executors;
import java.util.concurrent.atomic.AtomicInteger;
import org.junit.jupiter.api.AfterEach;
import org.junit.jupiter.api.BeforeAll;
import org.junit.jupiter.api.BeforeEach;
import org.junit.jupiter.api.Test;

class TokenVerifierTest {

    private static final String KID = "vektor-1";
    private static final String ISSUER = "vektor-auth";

    private static KeyPair pair;
    private static String jwksBody;

    private HttpServer server;
    private final AtomicInteger status = new AtomicInteger(200);
    private final AtomicInteger requests = new AtomicInteger();
    private String jwksUrl;

    @BeforeAll
    static void keys() throws Exception {
        var generator = KeyPairGenerator.getInstance("RSA");
        generator.initialize(2048);
        pair = generator.generateKeyPair();
        var jwk = new RSAKey.Builder(
            (java.security.interfaces.RSAPublicKey) pair.getPublic()
        )
            .keyID(KID)
            .build();
        jwksBody = new JWKSet(jwk).toString();
    }

    @BeforeEach
    void serveJwks() throws Exception {
        server = HttpServer.create(new InetSocketAddress("127.0.0.1", 0), 0);
        server.createContext("/jwks", exchange -> {
            requests.incrementAndGet();
            var body = jwksBody.getBytes(StandardCharsets.UTF_8);
            exchange
                .getResponseHeaders()
                .add("Content-Type", "application/json");
            exchange.sendResponseHeaders(status.get(), body.length);
            try (var out = exchange.getResponseBody()) {
                out.write(body);
            }
        });
        server.setExecutor(Executors.newFixedThreadPool(2));
        server.start();
        jwksUrl = "http://127.0.0.1:" + server.getAddress().getPort() + "/jwks";
    }

    @AfterEach
    void stop() {
        server.stop(0);
    }

    @Test
    void adminScopesGrantEmployeeAccess() throws Exception {
        // Регрессия: раньше роль сравнивалась напрямую и admin получал 403,
        // хотя auth выдаёт ему аудиторию employee.
        var verifier = verifier(Duration.ofMinutes(5), Duration.ofSeconds(30));
        var claims = verifier.verify(token("admin", ISSUER, 60));

        assertTrue(claims.employee());
        assertTrue(claims.canRead("employee"));
        assertTrue(claims.canRead("public"));
        assertEquals("kb", claims.kb());
        assertEquals("tenant", claims.tenant());
    }

    @Test
    void userWithoutEmployeeScopeIsNotEmployee() {
        var verifier = verifier(Duration.ofMinutes(5), Duration.ofSeconds(30));
        var token = tokenWithScopes("user", List.of("public"));

        var claims = verifier.verify(token);

        assertFalse(claims.employee());
        assertFalse(claims.canRead("employee"));
    }

    @Test
    void foreignIssuerRejected() throws Exception {
        var verifier = verifier(Duration.ofMinutes(5), Duration.ofSeconds(30));

        assertThrows(Errors.AppError.class, () ->
            verifier.verify(token("employee", "someone-else", 60))
        );
    }

    @Test
    void expiredTokenRejected() throws Exception {
        var verifier = verifier(Duration.ofMinutes(5), Duration.ofSeconds(30));

        assertThrows(Errors.AppError.class, () ->
            verifier.verify(token("employee", ISSUER, -1))
        );
    }

    @Test
    void unavailableAuthDoesNotStallEveryRequest() throws Exception {
        // TTL = 0: ключи всегда «просрочены», поэтому каждый вызов идёт в сеть,
        // если нет паузы после сбоя.
        var verifier = verifier(Duration.ZERO, Duration.ofSeconds(30));
        assertTrue(verifier.verify(token("employee", ISSUER, 60)).employee());

        status.set(500);
        for (int i = 0; i < 10; i++) {
            assertTrue(
                verifier.verify(token("employee", ISSUER, 60)).employee()
            );
        }

        assertTrue(
            requests.get() <= 3,
            "auth недоступен, но запросов к нему " + requests.get()
        );
    }

    private TokenVerifier verifier(Duration ttl, Duration backoff) {
        return new TokenVerifier(
            jwksUrl,
            ISSUER,
            HttpClient.newHttpClient(),
            ttl,
            backoff
        );
    }

    private static String token(String role, String issuer, long ttlSeconds)
        throws Exception {
        return tokenWithScopesAndIssuer(
            role,
            List.of("public", "employee"),
            issuer,
            ttlSeconds
        );
    }

    private static String tokenWithScopes(String role, List<String> scopes) {
        try {
            return tokenWithScopesAndIssuer(role, scopes, ISSUER, 60);
        } catch (Exception exception) {
            throw new IllegalStateException(exception);
        }
    }

    private static String tokenWithScopesAndIssuer(
        String role,
        List<String> scopes,
        String issuer,
        long ttlSeconds
    ) throws Exception {
        var claims = new JWTClaimsSet.Builder()
            .issuer(issuer)
            .subject("id-1")
            .claim("role", role)
            .claim("scp", scopes)
            .claim("tenant", "tenant")
            .claim("kb", "kb")
            .expirationTime(Date.from(Instant.now().plusSeconds(ttlSeconds)))
            .build();
        var jwt = new SignedJWT(
            new JWSHeader.Builder(JWSAlgorithm.RS256).keyID(KID).build(),
            claims
        );
        jwt.sign(new RSASSASigner((RSAPrivateKey) pair.getPrivate()));
        return jwt.serialize();
    }
}
