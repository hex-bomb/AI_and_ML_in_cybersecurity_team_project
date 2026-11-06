package ru.vektor.auth;

import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertFalse;
import static org.junit.jupiter.api.Assertions.assertSame;
import static org.junit.jupiter.api.Assertions.assertThrows;
import static org.junit.jupiter.api.Assertions.assertTrue;

import java.security.KeyPairGenerator;
import java.util.Base64;
import java.util.List;
import org.junit.jupiter.api.BeforeAll;
import org.junit.jupiter.api.Test;

class TokensTest {

    private static final String KID = "vektor-1";
    private static Tokens tokens;

    @BeforeAll
    static void setUp() throws Exception {
        var generator = KeyPairGenerator.getInstance("RSA");
        generator.initialize(2048);
        var pair = generator.generateKeyPair();
        tokens = new Tokens(
            Base64.getEncoder().encodeToString(pair.getPrivate().getEncoded()),
            KID,
            "vektor-auth"
        );
    }

    @Test
    void issuedTokenVerifies() throws Exception {
        var token = tokens.issue(
            "id-1",
            "Иван",
            "employee",
            List.of("public", "employee"),
            List.of("legal"),
            60
        );

        var claims = tokens.verify(token);

        assertEquals("employee", claims.getClaim("role"));
        assertEquals(List.of("public", "employee"), claims.getClaim("scp"));
        assertEquals(List.of("legal"), claims.getClaim("grp"));
    }

    @Test
    void expiredTokenRejected() throws Exception {
        var token = tokens.issue(
            "id-1",
            "Иван",
            "employee",
            List.of("public"),
            List.of(),
            -1
        );

        assertThrows(IllegalStateException.class, () -> tokens.verify(token));
    }

    @Test
    void jwksIsPublicAndPrecomputed() {
        var jwks = tokens.jwks();

        assertTrue(jwks.contains("\"kid\":\"" + KID + "\""));
        assertFalse(
            jwks.contains("\"d\""),
            "приватная экспонента не должна попасть в JWKS"
        );
        assertSame(jwks, tokens.jwks(), "JWKS должен быть посчитан один раз");
    }
}
