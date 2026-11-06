package ru.vektor.docs;

import com.fasterxml.jackson.databind.ObjectMapper;
import java.nio.charset.StandardCharsets;
import java.security.MessageDigest;
import java.security.NoSuchAlgorithmException;
import java.util.HashMap;
import java.util.HexFormat;
import java.util.List;
import java.util.Map;
import java.util.Optional;
import java.util.UUID;
import org.springframework.jdbc.core.simple.JdbcClient;
import org.springframework.stereotype.Component;
import org.springframework.transaction.annotation.Transactional;

/**
 * Хранилище документов и версий.
 *
 * <p>Каждый метод, который меняет состояние, пишет событие в {@code outbox_events}
 * в той же транзакции. Отправка в Kafka вынесена в {@link OutboxRelay}: падение
 * между коммитом и отправкой не теряет событие.
 */
@Component
public class DocumentRepository {

    public record Version(UUID id, String sha256, boolean created) {}

    public record Source(
        String externalId,
        String title,
        String section,
        String publication,
        UUID textBlobId
    ) {}

    public record Entry(
        String documentId,
        String title,
        String section,
        String publication,
        int versions
    ) {}

    public record Publication(
        String externalId,
        UUID versionId,
        List<String> channels,
        List<String> groups
    ) {}

    /**
     * Привязка страницы к документу: «этот блок сайта рассказывает про этот
     * документ». Номер версии нужен, чтобы показать расхождение с опубликованной.
     */
    public record Binding(
        String externalId,
        UUID documentRef,
        UUID versionId,
        String sectionKey
    ) {}

    private record ReindexSource(
        UUID documentId,
        String externalId,
        String title,
        String section,
        UUID versionId,
        int generation,
        String sha256
    ) {}

    /**
     * Один предикат для всех чтений: список, источник, поиск — везде одинаково.
     * Грант без {@code document_id} действует на все документы, без
     * {@code group_key} — на всех пользователей канала.
     */
    private static final String READABLE = """
    exists (
        select 1 from access_grants g
         where (g.document_id = d.id or g.document_id is null)
           and g.channel in (:channels)
           and (g.group_key is null or g.group_key in (:groups))
    )
    """;

    private final JdbcClient jdbc;
    private final ObjectMapper json;

    public DocumentRepository(JdbcClient jdbc, ObjectMapper json) {
        this.jdbc = jdbc;
        this.json = json;
    }

    @Transactional
    public Version upsert(
        String externalId,
        String title,
        String section,
        UUID blobId,
        UUID textBlobId,
        String sha,
        String versionLabel,
        String parser,
        String parserVersion,
        int textChars,
        String quality,
        String channel
    ) {
        var documentId = jdbc
            .sql(
                """
                insert into documents (id, external_id, title, section)
                values (:id, :external, :title, :section)
                on conflict (external_id) do update
                   set title = excluded.title,
                       section = excluded.section
                returning id
                """
            )
            .param("id", UUID.randomUUID())
            .param("external", externalId)
            .param("title", title)
            .param("section", section)
            .query(UUID.class)
            .single();

        var inserted = jdbc
            .sql(
                """
                insert into document_versions
                    (id, document_id, blob_id, text_blob_id, content_sha256,
                     version_label, index_generation)
                values (:id, :document, :blob, :textBlob, :sha, :label, 1)
                on conflict (document_id, content_sha256) do nothing
                returning id
                """
            )
            .param("id", UUID.randomUUID())
            .param("document", documentId)
            .param("blob", blobId)
            .param("textBlob", textBlobId)
            .param("sha", sha)
            .param("label", versionLabel)
            .query(UUID.class)
            .optional();

        var versionId = inserted.orElseGet(() ->
            jdbc
                .sql(
                    """
                    select id from document_versions
                     where document_id = :document and content_sha256 = :sha
                    """
                )
                .param("document", documentId)
                .param("sha", sha)
                .query(UUID.class)
                .single()
        );

        replaceChannelGrant(documentId, channel);

        if (inserted.isPresent()) {
            jdbc.sql(
                """
                insert into extraction_runs
                    (id, version_id, parser, parser_version, text_chars, quality)
                values (:id, :version, :parser, :parserVersion, :chars, :quality)
                """
            )
                .param("id", UUID.randomUUID())
                .param("version", versionId)
                .param("parser", parser)
                .param("parserVersion", parserVersion)
                .param("chars", textChars)
                .param("quality", quality)
                .update();

            jdbc.sql(
                """
                insert into index_generations (id, version_id, generation, state)
                values (:id, :version, 1, 'building')
                """
            )
                .param("id", UUID.randomUUID())
                .param("version", versionId)
                .update();

            enqueue(
                "document.version.created",
                documentId,
                Map.of(
                    "document_id",
                    documentId.toString(),
                    "external_id",
                    externalId,
                    "version_id",
                    versionId.toString(),
                    "generation",
                    1,
                    "title",
                    title,
                    "section",
                    section,
                    "content_sha256",
                    sha
                )
            );
        } else if (isPublished(documentId)) {
            // Канал мог измениться при повторной загрузке того же текста.
            enqueue(
                "document.access.changed",
                documentId,
                accessPayload(documentId)
            );
        }

        return new Version(versionId, sha, inserted.isPresent());
    }

    /**
     * Переиндексировать опубликованные документы.
     *
     * <p>Индекс Elasticsearch производный и в резервную копию не входит: после
     * восстановления его собирают заново. Номер генерации увеличивается, иначе
     * отчёт индексатора будет отклонён как устаревший.
     *
     * @return сколько документов поставлено в очередь
     */
    @Transactional
    public int reindexPublished() {
        var sources = jdbc
            .sql(
                """
                select d.id as document_id, d.external_id, d.title, d.section,
                       v.id as version_id, v.index_generation, v.content_sha256
                  from documents d
                  join document_versions v on v.id = d.published_version_id
                 where d.publication = 'published'
                   and d.lifecycle = 'active'
                   and v.processing = 'ready'
                 order by d.external_id
                """
            )
            .query((rs, row) ->
                new ReindexSource(
                    rs.getObject("document_id", UUID.class),
                    rs.getString("external_id"),
                    rs.getString("title"),
                    rs.getString("section"),
                    rs.getObject("version_id", UUID.class),
                    rs.getInt("index_generation"),
                    rs.getString("content_sha256")
                )
            )
            .list();

        for (var source : sources) {
            var generation = source.generation() + 1;
            // Брошенная сборка держала бы уникальный индекс «одна сборка на
            // версию» и не дала бы поставить новую.
            jdbc.sql(
                """
                update index_generations
                   set state = 'failed', finished_at = now()
                 where version_id = :version and state = 'building'
                """
            )
                .param("version", source.versionId())
                .update();
            jdbc.sql(
                """
                update document_versions set index_generation = :generation
                 where id = :version
                """
            )
                .param("generation", generation)
                .param("version", source.versionId())
                .update();
            // Строка этого номера могла остаться от прошлой неудачной сборки:
            // номер переиспользуем, иначе восстановление не поставит задание.
            jdbc.sql(
                """
                insert into index_generations (id, version_id, generation, state)
                values (:id, :version, :generation, 'building')
                on conflict (version_id, generation) do update
                   set state = 'building', started_at = now(), finished_at = null
                """
            )
                .param("id", UUID.randomUUID())
                .param("version", source.versionId())
                .param("generation", generation)
                .update();

            enqueue(
                "document.version.created",
                source.documentId(),
                Map.of(
                    "document_id",
                    source.documentId().toString(),
                    "external_id",
                    source.externalId(),
                    "version_id",
                    source.versionId().toString(),
                    "generation",
                    generation,
                    "title",
                    source.title(),
                    "section",
                    source.section(),
                    "content_sha256",
                    source.sha256()
                )
            );
            var payload = new HashMap<>(accessPayload(source.documentId()));
            payload.put("version_id", source.versionId().toString());
            payload.put("external_id", source.externalId());
            enqueue("document.published", source.documentId(), payload);
        }
        return sources.size();
    }

    /** Привязка страницы для установки виджета. Нет привязки — нет и подсказки. */
    public Binding binding(String installationKey, String contentKey) {
        return jdbc
            .sql(
                """
                select d.external_id, d.id as document_ref, d.published_version_id,
                       b.section_key
                  from page_bindings b
                  join widget_installations i on i.id = b.installation_id
                  join documents d on d.id = b.document_id
                 where i.key = :installation and i.is_active
                   and b.content_key = :content
                   and d.publication = 'published' and d.lifecycle = 'active'
                """
            )
            .param("installation", installationKey)
            .param("content", contentKey)
            .query((rs, row) ->
                new Binding(
                    rs.getString("external_id"),
                    rs.getObject("document_ref", UUID.class),
                    rs.getObject("published_version_id", UUID.class),
                    rs.getString("section_key")
                )
            )
            .optional()
            .orElse(null);
    }

    /**
     * Создать или заменить привязку. Установку создаёт {@code ensureInstallation}:
     * без неё привязку некуда положить.
     */
    @Transactional
    public void bindPage(
        String installationKey,
        String contentKey,
        String externalId,
        String sectionKey
    ) {
        var documentId = documentId(externalId);
        jdbc.sql(
            """
            insert into page_bindings
                (installation_id, content_key, document_id, section_key)
            select i.id, :content, :document, :section
              from widget_installations i where i.key = :installation
            on conflict (installation_id, content_key) do update
               set document_id = excluded.document_id,
                   section_key = excluded.section_key
            """
        )
            .param("content", contentKey)
            .param("document", documentId)
            .param("section", sectionKey)
            .param("installation", installationKey)
            .update();
    }

    /** Завести установку виджета, если её ещё нет. Повторный вызов ничего не меняет. */
    public void ensureInstallation(
        String key,
        String name,
        String channel,
        List<String> origins
    ) {
        jdbc.sql(
            """
            insert into widget_installations (key, name, channel, allowed_origins)
            values (:key, :name, :channel, :origins)
            on conflict (key) do nothing
            """
        )
            .param("key", key)
            .param("name", name)
            .param("channel", channel)
            .param("origins", origins.toArray(String[]::new))
            .update();
    }

    @Transactional
    public Publication publish(String externalId, UUID versionId) {
        var documentId = documentId(externalId);

        var target =
            versionId != null
                ? versionId
                : jdbc
                      .sql(
                          """
                          select id from document_versions
                           where document_id = :document and processing = 'ready'
                           order by created_at desc
                           limit 1
                          """
                      )
                      .param("document", documentId)
                      .query(UUID.class)
                      .optional()
                      .orElseThrow(Errors::notFound);

        var publishable = jdbc
            .sql(
                """
                select 1 from document_versions v
                  join documents d on d.id = v.document_id
                 where v.id = :version and v.document_id = :document
                   and v.processing = 'ready' and d.lifecycle = 'active'
                """
            )
            .param("version", target)
            .param("document", documentId)
            .query(Integer.class)
            .optional();
        if (publishable.isEmpty()) {
            throw Errors.badRequest(
                "версия не готова или принадлежит другому документу"
            );
        }

        // Проверки повторяет триггер: он остаётся последней линией защиты,
        // если этот запрос когда-нибудь обойдут.
        jdbc.sql(
            """
            update documents set published_version_id = :version
             where id = :document
            """
        )
            .param("version", target)
            .param("document", documentId)
            .update();

        var payload = new HashMap<>(accessPayload(documentId));
        payload.put("version_id", target.toString());
        payload.put("external_id", externalId);
        enqueue("document.published", documentId, payload);
        return new Publication(
            externalId,
            target,
            strings(payload.get("channels")),
            strings(payload.get("group_keys"))
        );
    }

    @Transactional
    public void withdraw(String externalId) {
        var documentId = documentId(externalId);
        jdbc.sql(
            """
            update documents
               set publication = 'withdrawn', published_version_id = null
             where id = :document and published_version_id is not null
            """
        )
            .param("document", documentId)
            .update();
        enqueue(
            "document.withdrawn",
            documentId,
            Map.of(
                "document_id",
                documentId.toString(),
                "external_id",
                externalId
            )
        );
    }

    /**
     * Отчёт индексатора. Принимается только для текущей генерации версии:
     * worker, доработавший после загрузки новой версии, не должен вернуть
     * документ в прежнее состояние.
     *
     * @return false, если отчёт относится к устаревшей генерации
     */
    @Transactional
    public boolean markIndexed(
        UUID versionId,
        int generation,
        String embeddingModel,
        int embeddingDim,
        int chunkCount
    ) {
        var current = jdbc
            .sql(
                """
                select index_generation from document_versions where id = :version
                """
            )
            .param("version", versionId)
            .query(Integer.class)
            .optional()
            .orElseThrow(Errors::notFound);

        if (current != generation) {
            return false;
        }

        jdbc.sql(
            """
            update document_versions
               set processing = 'ready', ready_at = now()
             where id = :version and processing <> 'ready'
            """
        )
            .param("version", versionId)
            .update();

        jdbc.sql(
            """
            insert into index_generations
                (id, version_id, generation, embedding_model, embedding_dim,
                 state, chunk_count, finished_at)
            values (:id, :version, :generation, :model, :dim, 'ready', :chunks, now())
            on conflict (version_id, generation) do update
               set state = 'ready',
                   embedding_model = excluded.embedding_model,
                   embedding_dim = excluded.embedding_dim,
                   chunk_count = excluded.chunk_count,
                   finished_at = now()
            """
        )
            .param("id", UUID.randomUUID())
            .param("version", versionId)
            .param("generation", generation)
            .param("model", embeddingModel)
            .param("dim", embeddingDim)
            .param("chunks", chunkCount)
            .update();

        return true;
    }

    public List<Entry> list(
        List<String> channels,
        List<String> groups,
        int limit
    ) {
        return jdbc
            .sql(
                """
                select d.external_id, d.title, d.section, d.publication,
                       count(v.id) as versions
                  from documents d
                  left join document_versions v on v.document_id = d.id
                 where d.lifecycle = 'active' and %s
                 group by d.id, d.external_id, d.title, d.section,
                          d.publication, d.created_at
                 order by d.created_at desc
                 limit :limit
                """.formatted(READABLE)
            )
            .param("channels", channels)
            .param("groups", groups)
            .param("limit", limit)
            .query((rs, row) ->
                new Entry(
                    rs.getString("external_id"),
                    rs.getString("title"),
                    rs.getString("section"),
                    rs.getString("publication"),
                    rs.getInt("versions")
                )
            )
            .list();
    }

    public Optional<Source> source(
        String externalId,
        List<String> channels,
        List<String> groups
    ) {
        return jdbc
            .sql(
                """
                select d.external_id, d.title, d.section, d.publication,
                       v.text_blob_id
                  from documents d
                  join document_versions v on v.id = d.published_version_id
                 where d.external_id = :external
                   and d.lifecycle = 'active'
                   and %s
                """.formatted(READABLE)
            )
            .param("external", externalId)
            .param("channels", channels)
            .param("groups", groups)
            .query((rs, row) ->
                new Source(
                    rs.getString("external_id"),
                    rs.getString("title"),
                    rs.getString("section"),
                    rs.getString("publication"),
                    rs.getObject("text_blob_id", UUID.class)
                )
            )
            .optional();
    }

    public Optional<UUID> textBlob(String externalId, UUID versionId) {
        return jdbc
            .sql(
                """
                select v.text_blob_id
                  from document_versions v
                  join documents d on d.id = v.document_id
                 where v.id = :version and d.external_id = :external
                """
            )
            .param("version", versionId)
            .param("external", externalId)
            .query(UUID.class)
            .optional();
    }

    public Optional<String> storageKey(UUID blobId) {
        return jdbc
            .sql("select storage_key from blobs where id = :id")
            .param("id", blobId)
            .query(String.class)
            .optional();
    }

    public UUID insertBlob(
        String storageKey,
        long size,
        String sha,
        String contentType
    ) {
        var id = UUID.randomUUID();
        jdbc.sql(
            """
            insert into blobs (id, storage_key, size_bytes, sha256, content_type)
            values (:id, :key, :size, :sha, :type)
            """
        )
            .param("id", id)
            .param("key", storageKey)
            .param("size", size)
            .param("sha", sha)
            .param("type", contentType)
            .update();
        return id;
    }

    public Optional<UUID> versionId(String externalId, String sha) {
        return jdbc
            .sql(
                """
                select v.id from document_versions v
                  join documents d on d.id = v.document_id
                 where d.external_id = :external and v.content_sha256 = :sha
                """
            )
            .param("external", externalId)
            .param("sha", sha)
            .query(UUID.class)
            .optional();
    }

    /** Смена канала без новой версии: переиндексировать нужно только опубликованную. */
    @Transactional
    public void setChannel(String externalId, String channel) {
        var documentId = documentId(externalId);
        replaceChannelGrant(documentId, channel);
        if (isPublished(documentId)) {
            enqueue(
                "document.access.changed",
                documentId,
                accessPayload(documentId)
            );
        }
    }

    private UUID documentId(String externalId) {
        return jdbc
            .sql(
                """
                select id from documents
                 where external_id = :external and lifecycle = 'active'
                """
            )
            .param("external", externalId)
            .query(UUID.class)
            .optional()
            .orElseThrow(Errors::notFound);
    }

    private void replaceChannelGrant(UUID documentId, String channel) {
        jdbc.sql(
            """
            delete from access_grants
             where document_id = :document and group_key is null
            """
        )
            .param("document", documentId)
            .update();
        jdbc.sql(
            """
            insert into access_grants (document_id, group_key, channel)
            values (:document, null, :channel)
            """
        )
            .param("document", documentId)
            .param("channel", channel)
            .update();
    }

    private boolean isPublished(UUID documentId) {
        return jdbc
            .sql(
                """
                select published_version_id is not null
                  from documents where id = :document
                """
            )
            .param("document", documentId)
            .query(Boolean.class)
            .single();
    }

    private Map<String, Object> accessPayload(UUID documentId) {
        var channels = jdbc
            .sql(
                """
                select distinct channel from access_grants
                 where document_id = :document or document_id is null
                 order by channel
                """
            )
            .param("document", documentId)
            .query(String.class)
            .list();
        var groups = jdbc
            .sql(
                """
                select distinct group_key from access_grants
                 where (document_id = :document or document_id is null)
                   and group_key is not null
                 order by group_key
                """
            )
            .param("document", documentId)
            .query(String.class)
            .list();
        return Map.of(
            "document_id",
            documentId.toString(),
            "channels",
            channels,
            "group_keys",
            groups
        );
    }

    private void enqueue(
        String type,
        UUID documentId,
        Map<String, Object> payload
    ) {
        try {
            jdbc.sql(
                """
                insert into outbox_events
                    (aggregate_type, aggregate_id, event_type, payload)
                values ('document', :id, :type, cast(:payload as jsonb))
                """
            )
                .param("id", documentId)
                .param("type", type)
                .param("payload", json.writeValueAsString(payload))
                .update();
        } catch (com.fasterxml.jackson.core.JsonProcessingException exception) {
            throw new IllegalStateException(
                "cannot serialize event",
                exception
            );
        }
    }

    private static List<String> strings(Object value) {
        return value instanceof List<?> list
            ? list.stream().map(String::valueOf).toList()
            : List.of();
    }

    static String sha256(String text) {
        try {
            var digest = MessageDigest.getInstance("SHA-256").digest(
                text.getBytes(StandardCharsets.UTF_8)
            );
            return HexFormat.of().formatHex(digest);
        } catch (NoSuchAlgorithmException exception) {
            throw new IllegalStateException(exception);
        }
    }
}
