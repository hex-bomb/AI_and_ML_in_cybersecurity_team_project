package ru.vektor.auth;

import org.springframework.beans.factory.annotation.Value;
import org.springframework.boot.ApplicationRunner;
import org.springframework.boot.SpringApplication;
import org.springframework.boot.autoconfigure.SpringBootApplication;
import org.springframework.context.annotation.Bean;
import org.springframework.context.annotation.Profile;

@SpringBootApplication
public class AuthApplication {

    public static void main(String[] args) {
        SpringApplication.run(AuthApplication.class, args);
    }

    /**
     * Демо-пользователи создаются только в dev-профиле.
     * В проде аккаунты заводятся вручную, пароли не равны логинам.
     */
    @Bean
    @Profile("dev")
    ApplicationRunner devUsers(
        UserRepository users,
        @Value("${auth.tenant-id}") String tenantId,
        @Value(
            "${auth.dev-admin-password:dev-admin-password}"
        ) String adminPassword
    ) {
        return args -> {
            users.seed("user", "Клиент", "user", tenantId, "user");
            users.seed(
                "employee",
                "Сотрудник",
                "employee",
                tenantId,
                "employee"
            );
            // Сервисный аккаунт: его токен живёт практически без срока (adminTtl).
            users.seed(
                "admin",
                "Администратор",
                "admin",
                tenantId,
                adminPassword
            );
        };
    }
}
