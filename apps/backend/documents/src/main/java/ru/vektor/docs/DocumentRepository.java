package ru.vektor.docs;

import java.nio.charset.StandardCharsets;
import java.security.MessageDigest;
import java.security.NoSuchAlgorithmException;
import java.util.HexFormat;
import java.util.List;
import java.util.Optional;
import java.util.UUID;

import org.springframework.jdbc.core.simple.JdbcClient;
import org.springframework.stereotype.Component;
import org.springframework.transaction.annotation.Transactional;

@Component
public class DocumentRepository {

    public record Version(UUID id, String sha256, boolean created) {
    }

    public record Source(String externalId, String title, String audience, String section, String text) {
    }

    public record Entry(String documentId, String title, String audience, String section, int versions) {
    }

    private final JdbcClient jdbc;

    public DocumentRepository(JdbcClient jdbc) {
        this.jdbc = jdbc;
    }

    /**
     * Сохраняет версию документа, создавая сам документ при первом появлении.
     *
     * <p>Версия уникальна по паре {@code (document_id, content_sha256)}: повторная
     * загрузка того же текста не плодит версии, а возвращает существующую
     * с {@code created = false}.
     */
    @Transactional
    public Version upsert(String kbId, String externalId, String title, String audience,
                          String section, String text, String versionLabel) {
        var sha = sha256(text);

        var documentId = jdbc.sql("""
                insert into documents (id, kb_id, external_id, title, audience, section)
                values (:id, :kb, :external, :title, :audience, :section)
                on conflict (kb_id, external_id) do update
                   set title = excluded.title,
                       audience = excluded.audience,
                       section = excluded.section
                returning id
                """)
                .param("id", UUID.randomUUID())
                .param("kb", kbId)
                .param("external", externalId)
                .param("title", title)
                .param("audience", audience)
                .param("section", section)
                .query(UUID.class)
                .single();

        var inserted = jdbc.sql("""
                insert into document_versions (id, document_id, content_sha256, version_label, full_text)
                values (:id, :document, :sha, :label, :text)
                on conflict (document_id, content_sha256) do nothing
                returning id
                """)
                .param("id", UUID.randomUUID())
                .param("document", documentId)
                .param("sha", sha)
                .param("label", versionLabel)
                .param("text", text)
                .query(UUID.class)
                .optional();

        var versionId = inserted.orElseGet(() -> jdbc.sql("""
                        select id from document_versions
                         where document_id = :document and content_sha256 = :sha
                        """)
                .param("document", documentId)
                .param("sha", sha)
                .query(UUID.class)
                .single());

        return new Version(versionId, sha, inserted.isPresent());
    }

    public Optional<Source> latest(String kbId, String externalId) {
        return jdbc.sql("""
                select d.external_id, d.title, d.audience, d.section, v.full_text
                  from documents d
                  join lateral (
                      select full_text
                        from document_versions
                       where document_id = d.id
                       order by created_at desc
                       limit 1
                  ) v on true
                 where d.kb_id = :kb and d.external_id = :external
                """)
                .param("kb", kbId)
                .param("external", externalId)
                .query((rs, row) -> new Source(
                        rs.getString("external_id"),
                        rs.getString("title"),
                        rs.getString("audience"),
                        rs.getString("section"),
                        rs.getString("full_text")))
                .optional();
    }

    public Optional<String> text(String externalId, UUID versionId) {
        return jdbc.sql("""
                select v.full_text
                  from document_versions v
                  join documents d on d.id = v.document_id
                 where v.id = :version and d.external_id = :external
                """)
                .param("version", versionId)
                .param("external", externalId)
                .query(String.class)
                .optional();
    }

    public List<Entry> list(String kbId, int limit) {
        return jdbc.sql("""
                select d.external_id, d.title, d.audience, d.section, count(v.id) as versions
                  from documents d
                  left join document_versions v on v.document_id = d.id
                 where d.kb_id = :kb
                 group by d.id, d.external_id, d.title, d.audience, d.section, d.created_at
                 order by d.created_at desc
                 limit :limit
                """)
                .param("kb", kbId)
                .param("limit", limit)
                .query((rs, row) -> new Entry(
                        rs.getString("external_id"),
                        rs.getString("title"),
                        rs.getString("audience"),
                        rs.getString("section"),
                        rs.getInt("versions")))
                .list();
    }

    static String sha256(String text) {
        try {
            var digest = MessageDigest.getInstance("SHA-256").digest(text.getBytes(StandardCharsets.UTF_8));
            return HexFormat.of().formatHex(digest);
        } catch (NoSuchAlgorithmException exception) {
            throw new IllegalStateException(exception);
        }
    }
}
