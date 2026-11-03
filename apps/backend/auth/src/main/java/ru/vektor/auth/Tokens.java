package ru.vektor.auth;

import com.nimbusds.jose.JWSAlgorithm;
import com.nimbusds.jose.JWSHeader;
import com.nimbusds.jose.crypto.RSASSASigner;
import com.nimbusds.jose.crypto.RSASSAVerifier;
import com.nimbusds.jose.jwk.JWKSet;
import com.nimbusds.jose.jwk.RSAKey;
import com.nimbusds.jwt.JWTClaimsSet;
import com.nimbusds.jwt.SignedJWT;
import java.security.KeyFactory;
import java.security.interfaces.RSAPrivateCrtKey;
import java.security.interfaces.RSAPublicKey;
import java.security.spec.PKCS8EncodedKeySpec;
import java.security.spec.RSAPublicKeySpec;
import java.time.Instant;
import java.time.temporal.ChronoUnit;
import java.util.Base64;
import java.util.Date;
import java.util.List;
import org.springframework.beans.factory.annotation.Value;
import org.springframework.stereotype.Component;

@Component
public class Tokens {

    private final RSAKey key;
    private final String issuer;
    // JWKS неизменен, пока жив ключ: сериализуем один раз, а не на каждый запрос.
    private final String jwksJson;

    public Tokens(
        @Value("${auth.signing-key-b64:}") String privateKeyB64,
        @Value("${auth.key-id}") String keyId,
        @Value("${auth.issuer}") String issuer
    ) throws Exception {
        if (privateKeyB64 == null || privateKeyB64.isBlank()) {
            throw new IllegalStateException(
                "auth.signing-key-b64 не задан: проверьте secret/vektor в Vault"
            );
        }
        var factory = KeyFactory.getInstance("RSA");
        var privateKey = (RSAPrivateCrtKey) factory.generatePrivate(
            new PKCS8EncodedKeySpec(Base64.getDecoder().decode(privateKeyB64))
        );
        var publicKey = (RSAPublicKey) factory.generatePublic(
            new RSAPublicKeySpec(
                privateKey.getModulus(),
                privateKey.getPublicExponent()
            )
        );
        this.key = new RSAKey.Builder(publicKey)
            .privateKey(privateKey)
            .keyID(keyId)
            .build();
        this.issuer = issuer;
        this.jwksJson = new JWKSet(key.toPublicJWK()).toString();
    }

    public String issue(
        String subject,
        String name,
        String role,
        List<String> scopes,
        String tenant,
        String kb,
        long ttlSeconds
    ) throws Exception {
        var now = Instant.now();
        var claims = new JWTClaimsSet.Builder()
            .issuer(issuer)
            .subject(subject)
            .claim("name", name)
            .claim("role", role)
            .claim("scp", scopes)
            .claim("tenant", tenant)
            .claim("kb", kb)
            .issueTime(Date.from(now))
            .expirationTime(Date.from(now.plus(ttlSeconds, ChronoUnit.SECONDS)))
            .build();

        var jwt = new SignedJWT(
            new JWSHeader.Builder(JWSAlgorithm.RS256)
                .keyID(key.getKeyID())
                .build(),
            claims
        );
        jwt.sign(new RSASSASigner(key));
        return jwt.serialize();
    }

    public JWTClaimsSet verify(String token) throws Exception {
        var jwt = SignedJWT.parse(token);
        if (!jwt.verify(new RSASSAVerifier(key.toPublicJWK()))) {
            throw new IllegalStateException("подпись не сходится");
        }
        var expiresAt = jwt.getJWTClaimsSet().getExpirationTime();
        if (expiresAt == null || expiresAt.before(new Date())) {
            throw new IllegalStateException("сессия истекла");
        }
        return jwt.getJWTClaimsSet();
    }

    public String jwks() {
        return jwksJson;
    }
}
