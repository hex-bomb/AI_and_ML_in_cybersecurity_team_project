package ru.vektor.auth;

import java.time.Clock;
import java.util.concurrent.ConcurrentHashMap;
import java.util.concurrent.atomic.AtomicInteger;
import java.util.concurrent.atomic.AtomicLong;
import org.slf4j.Logger;
import org.slf4j.LoggerFactory;
import org.springframework.beans.factory.annotation.Autowired;
import org.springframework.beans.factory.annotation.Value;
import org.springframework.stereotype.Component;

/**
 * Ограничитель попыток входа: счётчик на ключ (логин + IP) в окне {@code windowSeconds}.
 * Окно не скользит: счётчик обнуляется, когда с момента открытия окна прошло
 * {@code windowSeconds}. Состояние in-memory и теряется при рестарте — для стенда приемлемо.
 *
 * <p>Вход неаутентифицирован, поэтому карта ключей ограничена: иначе перебор
 * логинов превращает троттлинг в утечку памяти вплоть до OOM. Потокобезопасен:
 * состояние в {@link ConcurrentHashMap}, счётчики атомарные.
 */
@Component
public class LoginThrottle {

    private static final Logger log = LoggerFactory.getLogger(
        LoginThrottle.class
    );

    /** Предел числа окон: защита от роста карты при переборе уникальных логинов. */
    static final int MAX_KEYS = 10_000;

    /** Полный проход по карте раз в столько вызовов: на каждом входе это слишком дорого. */
    private static final long SWEEP_EVERY = 1024;

    record Attempt(long windowStart, AtomicInteger count) {}

    private final ConcurrentHashMap<String, Attempt> attempts =
        new ConcurrentHashMap<>();
    private final AtomicLong calls = new AtomicLong();
    private final int maxAttempts;
    private final long windowSeconds;
    private final Clock clock;

    @Autowired
    public LoginThrottle(
        @Value("${auth.throttle-max-attempts:5}") int maxAttempts,
        @Value("${auth.throttle-window-seconds:300}") long windowSeconds
    ) {
        this(maxAttempts, windowSeconds, Clock.systemUTC());
    }

    LoginThrottle(int maxAttempts, long windowSeconds, Clock clock) {
        this.maxAttempts = maxAttempts;
        this.windowSeconds = windowSeconds;
        this.clock = clock;
    }

    /**
     * true — попытка разрешена, false — лимит окна исчерпан. Счётчик не меняет:
     * неудачу нужно отметить через {@link #recordFailure(String)}. Раз в
     * {@code SWEEP_EVERY} вызовов попутно чистит просроченные окна.
     */
    public boolean allowed(String key) {
        if (calls.incrementAndGet() % SWEEP_EVERY == 0) {
            prune();
        }
        return state(key).count.get() < maxAttempts;
    }

    public void recordFailure(String key) {
        state(key).count.incrementAndGet();
    }

    public void reset(String key) {
        attempts.remove(key);
    }

    private Attempt state(String key) {
        long now = clock.instant().getEpochSecond();
        if (attempts.size() >= MAX_KEYS) {
            prune();
            if (attempts.size() >= MAX_KEYS) {
                // Освобождаем память ценой сброса окон: доступность важнее
                // удержания блокировки, которую иначе можно навязать извне.
                log.warn(
                    "login throttle: {} ключей, сбрасываем окна",
                    attempts.size()
                );
                attempts.clear();
            }
        }
        return attempts.compute(key, (k, previous) -> {
            if (
                previous == null ||
                now - previous.windowStart() >= windowSeconds
            ) {
                return new Attempt(now, new AtomicInteger(0));
            }
            return previous;
        });
    }

    void prune() {
        long cutoff = clock.instant().getEpochSecond() - windowSeconds;
        attempts.values().removeIf(a -> a.windowStart() < cutoff);
    }

    /** Число окон в карте — для тестов и диагностики. */
    int size() {
        return attempts.size();
    }
}
