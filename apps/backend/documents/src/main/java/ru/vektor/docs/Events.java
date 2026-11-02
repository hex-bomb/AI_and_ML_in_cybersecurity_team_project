package ru.vektor.docs;

import com.fasterxml.jackson.databind.ObjectMapper;
import java.util.Map;
import org.slf4j.Logger;
import org.slf4j.LoggerFactory;
import org.springframework.beans.factory.annotation.Value;
import org.springframework.kafka.core.KafkaTemplate;
import org.springframework.stereotype.Component;

@Component
public class Events {

    private static final Logger log = LoggerFactory.getLogger(Events.class);

    private final KafkaTemplate<String, String> kafka;
    private final ObjectMapper json;
    private final String topic;

    public Events(
        KafkaTemplate<String, String> kafka,
        ObjectMapper json,
        @Value("${documents.topic}") String topic
    ) {
        this.kafka = kafka;
        this.json = json;
        this.topic = topic;
    }

    public void upserted(
        String kbId,
        String externalId,
        String versionId,
        String audience,
        String title,
        String sha256
    ) {
        try {
            var payload = json.writeValueAsString(
                Map.of(
                    "event",
                    "document.upserted",
                    "document_id",
                    externalId,
                    "version_id",
                    versionId,
                    "kb_id",
                    kbId,
                    "audience",
                    audience,
                    "title",
                    title == null ? "" : title,
                    "content_sha256",
                    sha256
                )
            );
            // send() асинхронен: реальная ошибка брокера приходит в callback,
            // без него потерянное событие было бы невидимо.
            kafka
                .send(topic, externalId, payload)
                .whenComplete((result, error) -> {
                    if (error != null) {
                        // Индекс производный: потерянное событие чинится повторной публикацией.
                        log.error(
                            "publish failed document_id={}",
                            externalId,
                            error
                        );
                    }
                });
        } catch (Exception exception) {
            log.error("publish failed document_id={}", externalId, exception);
        }
    }
}
