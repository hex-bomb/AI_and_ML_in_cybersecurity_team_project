package ru.vektor.auth;

import java.util.List;
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
        String login,
        String name,
        String role,
        String passwordHash
    ) {}

    private static final String SELECT = """
    select id, login, name, role, password_hash
      from users
     where login = :login and is_active
    """;

    private static final String INSERT = """
    insert into users (id, login, name, role, password_hash)
    values (:id, :login, :name, :role, :hash)
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
                    rs.getString("login"),
                    rs.getString("name"),
                    rs.getString("role"),
                    rs.getString("password_hash")
                )
            )
            .optional();
    }

    /** Группы пользователя: строковые ключи, которые понимают другие сервисы. */
    public List<String> groups(UUID userId) {
        return jdbc
            .sql(
                """
                select g.key
                  from memberships m
                  join groups g on g.id = m.group_id
                 where m.user_id = :id
                 order by g.key
                """
            )
            .param("id", userId)
            .query(String.class)
            .list();
    }

    public void seed(
        String login,
        String name,
        String role,
        String rawPassword
    ) {
        if (find(login).isPresent()) {
            return;
        }
        jdbc.sql(INSERT)
            .param("id", UUID.randomUUID())
            .param("login", login)
            .param("name", name)
            .param("role", role)
            .param("hash", encoder.encode(rawPassword))
            .update();
    }
}
