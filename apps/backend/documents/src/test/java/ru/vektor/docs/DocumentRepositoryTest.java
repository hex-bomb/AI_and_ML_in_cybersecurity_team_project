package ru.vektor.docs;

import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertFalse;
import static org.junit.jupiter.api.Assertions.assertThrows;
import static org.junit.jupiter.api.Assertions.assertTrue;

import com.fasterxml.jackson.databind.ObjectMapper;
import java.util.List;
import java.util.UUID;
import java.util.function.Supplier;
import javax.sql.DataSource;
import org.flywaydb.core.Flyway;
import org.junit.jupiter.api.BeforeAll;
import org.junit.jupiter.api.BeforeEach;
import org.junit.jupiter.api.Test;
import org.springframework.jdbc.core.simple.JdbcClient;
import org.springframework.jdbc.datasource.DataSourceTransactionManager;
import org.springframework.jdbc.datasource.DriverManagerDataSource;
import org.springframework.transaction.support.TransactionTemplate;
import org.testcontainers.containers.PostgreSQLContainer;
import org.testcontainers.junit.jupiter.Container;
import org.testcontainers.junit.jupiter.Testcontainers;

/**
 * Проверяет SQL репозитория и инварианты схемы на настоящем PostgreSQL:
 * триггеры и уникальные индексы невозможно проверить на моках.
 */
@Testcontainers
class DocumentRepositoryTest {

    @Container
    static final PostgreSQLContainer<?> DATABASE = new PostgreSQLContainer<>(
        "postgres:16-alpine"
    );

    private static DocumentRepository repository;
    private static JdbcClient jdbc;
    private static TransactionTemplate tx;

    @BeforeAll
    static void migrate() {
        DataSource dataSource = new DriverManagerDataSource(
            DATABASE.getJdbcUrl(),
            DATABASE.getUsername(),
            DATABASE.getPassword()
        );
        Flyway.configure()
            .dataSource(dataSource)
            .locations("classpath:db/migration")
            .load()
            .migrate();
        jdbc = JdbcClient.create(dataSource);
        repository = new DocumentRepository(jdbc, new ObjectMapper());
        tx = new TransactionTemplate(
            new DataSourceTransactionManager(dataSource)
        );
    }

    private static <T> T inTx(Supplier<T> work) {
        return tx.execute(status -> work.get());
    }

    // Тесты делят один контейнер: без очистки счётчики outbox зависят от порядка.
    @BeforeEach
    void clean() {
        jdbc.sql(
            "truncate documents, blobs, outbox_events restart identity cascade"
        ).update();
    }

    private DocumentRepository.Version store(
        String externalId,
        String text,
        String channel
    ) {
        var sha = DocumentRepository.sha256(text);
        return inTx(() -> {
            var blob = repository.insertBlob(
                "blob-" + UUID.randomUUID(),
                text.length(),
                sha,
                "text/plain"
            );
            return repository.upsert(
                externalId,
                "Документ " + externalId,
                "Тест",
                blob,
                blob,
                sha,
                "v1",
                "test",
                "1",
                text.length(),
                "ok",
                channel
            );
        });
    }

    private long outbox() {
        return jdbc
            .sql(
                "select count(*) from outbox_events where published_at is null"
            )
            .query(Long.class)
            .single();
    }

    @Test
    void createsVersionGenerationAndEvent() {
        var version = store(
            "doc-create",
            "Текст документа для проверки",
            "public"
        );

        assertTrue(version.created());
        assertEquals(1, outbox());
        assertEquals(
            "building",
            jdbc
                .sql(
                    "select state from index_generations where version_id = :id"
                )
                .param("id", version.id())
                .query(String.class)
                .single()
        );
    }

    @Test
    void repeatedUploadReturnsSameVersion() {
        var text = "Один и тот же текст дважды";
        var first = store("doc-repeat", text, "public");
        var second = store("doc-repeat", text, "public");

        assertTrue(first.created());
        assertFalse(second.created());
        assertEquals(first.id(), second.id());
        // Второе событие не нужно: индекс уже построен по этой версии.
        assertEquals(1, outbox());
    }

    @Test
    void publishRequiresReadyVersion() {
        var version = store("doc-publish", "Документ для публикации", "public");

        assertThrows(Errors.AppError.class, () ->
            inTx(() -> repository.publish("doc-publish", null))
        );

        assertTrue(
            inTx(() -> repository.markIndexed(version.id(), 1, "e5", 1024, 3))
        );

        var publication = inTx(() -> repository.publish("doc-publish", null));
        assertEquals(version.id(), publication.versionId());
        assertEquals(List.of("public"), publication.channels());
        // Событие публикации должно нести версию: без неё ингест не поймёт,
        // какие чанки открывать.
        var payload = jdbc
            .sql(
                """
                select payload::text from outbox_events
                 where event_type = 'document.published'
                """
            )
            .query(String.class)
            .single();
        assertTrue(payload.contains(version.id().toString()));
        assertEquals(
            "published",
            jdbc
                .sql(
                    "select publication from documents where external_id = :id"
                )
                .param("id", "doc-publish")
                .query(String.class)
                .single()
        );
    }

    @Test
    void staleGenerationReportIsRejected() {
        var version = store(
            "doc-stale",
            "Документ с устаревшей генерацией",
            "public"
        );

        assertTrue(
            inTx(() -> repository.markIndexed(version.id(), 1, "e5", 1024, 2))
        );
        // Отчёт второй генерации относится к переиндексации, которой не было.
        assertFalse(
            inTx(() -> repository.markIndexed(version.id(), 2, "e5", 1024, 2))
        );
    }

    @Test
    void listRespectsChannels() {
        store("doc-public", "Публичный документ корпуса", "public");
        store("doc-internal", "Внутренний документ корпуса", "internal");

        var asVisitor = repository.list(
            List.of("public"),
            List.of("__none__"),
            100
        );
        var asEmployee = repository.list(
            List.of("public", "internal"),
            List.of("__none__"),
            100
        );

        assertTrue(
            asVisitor
                .stream()
                .anyMatch(e -> e.documentId().equals("doc-public"))
        );
        assertTrue(
            asVisitor
                .stream()
                .noneMatch(e -> e.documentId().equals("doc-internal"))
        );
        assertTrue(
            asEmployee
                .stream()
                .anyMatch(e -> e.documentId().equals("doc-internal"))
        );
    }

    @Test
    void withdrawnDocumentDisappearsFromSource() {
        var version = store("doc-withdraw", "Документ для отзыва", "public");
        inTx(() -> repository.markIndexed(version.id(), 1, "e5", 1024, 1));
        inTx(() -> repository.publish("doc-withdraw", null));

        assertTrue(
            repository
                .source("doc-withdraw", List.of("public"), List.of("__none__"))
                .isPresent()
        );

        inTx(() -> {
            repository.withdraw("doc-withdraw");
            return null;
        });

        assertTrue(
            repository
                .source("doc-withdraw", List.of("public"), List.of("__none__"))
                .isEmpty()
        );
    }
}
