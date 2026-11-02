package ru.vektor.auth;

import java.util.Optional;
import java.util.UUID;
import org.springframework.jdbc.core.simple.JdbcClient;
import org.springframework.security.crypto.password.PasswordEncoder;
import org.springframework.security.crypto.scrypt.SCryptPasswordEncoder;
import org.springframework.stereotype.Component;

@Component
public class UserRepository {

    public record User(
        UUID id,
        UUID tenantId,
        String login,
        String name,
        String role,
        String passwordHash
    ) {}

    private static final String SELECT = """
    select id, tenant_id, login, name, role, password_hash
      from auth_users
     where login = :login
    """;

    private static final String INSERT = """
    insert into auth_users (id, tenant_id, login, name, role, password_hash)
    values (:id, :tenant, :login, :name, :role, :hash)
    on conflict (login) do nothing
    """;

    private final JdbcClient jdbc;
    final PasswordEncoder encoder =
        SCryptPasswordEncoder.defaultsForSpringSecurity_v5_8();

    public UserRepository(JdbcClient jdbc) {
        this.jdbc = jdbc;
    }

    public Optional<User> find(String login) {
        return jdbc
            .sql(SELECT)
            .param("login", login)
            .query((rs, row) ->
                new User(
                    rs.getObject("id", UUID.class),
                    rs.getObject("tenant_id", UUID.class),
                    rs.getString("login"),
                    rs.getString("name"),
                    rs.getString("role"),
                    rs.getString("password_hash")
                )
            )
            .optional();
    }

    public void seed(
        String login,
        String name,
        String role,
        String tenantId,
        String rawPassword
    ) {
        if (find(login).isPresent()) {
            return;
        }
        jdbc.sql(INSERT)
            .param("id", UUID.randomUUID())
            .param("tenant", UUID.fromString(tenantId))
            .param("login", login)
            .param("name", name)
            .param("role", role)
            .param("hash", encoder.encode(rawPassword))
            .update();
    }
}
