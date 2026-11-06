package ru.vektor.docs;

import com.fasterxml.jackson.databind.ObjectMapper;
import com.fasterxml.jackson.databind.node.ObjectNode;
import java.util.UUID;
import java.util.concurrent.TimeUnit;
import org.slf4j.Logger;
import org.slf4j.LoggerFactory;
import org.springframework.beans.factory.annotation.Value;
import org.springframework.jdbc.core.simple.JdbcClient;
import org.springframework.kafka.core.KafkaTemplate;
import org.springframework.scheduling.annotation.Scheduled;
import org.springframework.stereotype.Component;

/**
 * Публикует события из outbox в Kafka.
 *
 * <p>Транзакция БД и отправка в брокер не атомарны, поэтому семантика
 * at-least-once: событие может уйти повторно, если процесс упал между
 * отправкой и отметкой {@code published_at}. Потребители обязаны быть
 * идемпотентны по идентификатору события.
 */
@Component
public class OutboxRelay {

    private static final Logger log = LoggerFactory.getLogger(
        OutboxRelay.class
    );

    private record Pending(
        long id,
        UUID aggregateId,
        String type,
        String payload
    ) {}

    private final JdbcClient jdbc;
    private final KafkaTemplate<String, String> kafka;
    private final ObjectMapper mapper;
    private final String topic;
    private final int batchSize;

    public OutboxRelay(
        JdbcClient jdbc,
        KafkaTemplate<String, String> kafka,
        ObjectMapper mapper,
        @Value("${documents.topic}") String topic,
        @Value("${documents.outbox-batch:100}") int batchSize
    ) {
        this.jdbc = jdbc;
        this.kafka = kafka;
        this.mapper = mapper;
        this.topic = topic;
        this.batchSize = batchSize;
    }

    @Scheduled(fixedDelayString = "${documents.outbox-interval-ms:500}")
    public void flush() {
        var pending = jdbc
            .sql(
                """
                select id, aggregate_id, event_type, payload::text as payload
                  from outbox_events
                 where published_at is null
                 order by id
                 limit :limit
                """
            )
            .param("limit", batchSize)
            .query((rs, row) ->
                new Pending(
                    rs.getLong("id"),
                    rs.getObject("aggregate_id", UUID.class),
                    rs.getString("event_type"),
                    rs.getString("payload")
                )
            )
            .list();

        for (var event : pending) {
            try {
                // Конверт: тип и идентификатор события отдельно от полей
                // документа. Ingest разбирает только конверт и не гадает
                // по форме payload.
                ObjectNode envelope = mapper.readValue(
                    event.payload(),
                    ObjectNode.class
                );
                envelope.put("event", event.type());
                envelope.put("event_id", String.valueOf(event.id()));
                // Синхронно: следующий батч не должен обгонять предыдущий,
                // иначе порядок изменений одного документа потеряется.
                kafka
                    .send(
                        topic,
                        event.aggregateId().toString(),
                        mapper.writeValueAsString(envelope)
                    )
                    .get(5, TimeUnit.SECONDS);
                jdbc.sql(
                    """
                    update outbox_events
                       set published_at = now(), attempts = attempts + 1
                     where id = :id
                    """
                )
                    .param("id", event.id())
                    .update();
            } catch (Exception exception) {
                if (exception instanceof InterruptedException) {
                    Thread.currentThread().interrupt();
                }
                jdbc.sql(
                    """
                    update outbox_events
                       set attempts = attempts + 1, last_error = :error
                     where id = :id
                    """
                )
                    .param("id", event.id())
                    .param("error", String.valueOf(exception.getMessage()))
                    .update();
                log.warn(
                    "outbox publish failed id={} type={}",
                    event.id(),
                    event.type(),
                    exception
                );
                // Брокер недоступен: остальные события подождут следующего тика.
                return;
            }
        }
    }

    /** Для тестов и диагностики: сколько событий ещё не опубликовано. */
    public long pending() {
        return jdbc
            .sql(
                "select count(*) from outbox_events where published_at is null"
            )
            .query(Long.class)
            .single();
    }
}
