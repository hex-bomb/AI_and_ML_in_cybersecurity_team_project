package ru.vektor.auth;

import java.time.Clock;
import java.util.concurrent.ConcurrentHashMap;
import java.util.concurrent.atomic.AtomicInteger;
import org.springframework.beans.factory.annotation.Autowired;
import org.springframework.beans.factory.annotation.Value;
import org.springframework.stereotype.Component;

/**
 * Ограничитель попыток входа: скользящее окно на ключ (логин + IP).
 * Состояние in-memory и теряется при рестарте — для стенда приемлемо.
 */
@Component
public class LoginThrottle {

    record Attempt(long windowStart, AtomicInteger count) {}

    private final ConcurrentHashMap<String, Attempt> attempts =
        new ConcurrentHashMap<>();
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

    /** true — попытка разрешена, false — превышен лимит. */
    public boolean allowed(String key) {
        return state(key).count.get() < maxAttempts;
    }

    public void recordFailure(String key) {
        state(key).count.incrementAndGet();
    }

    public void reset(String key) {
        attempts.remove(key);
    }

    private Attempt state(String key) {
        return attempts.compute(key, (k, previous) -> {
            long now = clock.instant().getEpochSecond();
            if (
                previous == null ||
                now - previous.windowStart() >= windowSeconds
            ) {
                return new Attempt(now, new AtomicInteger(0));
            }
            return previous;
        });
    }

    /** Устаревшие окна не должны копиться бесконечно. */
    void prune() {
        long cutoff = clock.instant().getEpochSecond() - windowSeconds;
        attempts.values().removeIf(a -> a.windowStart() < cutoff);
    }
}
