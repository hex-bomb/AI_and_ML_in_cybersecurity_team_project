package ru.vektor.auth;

import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertFalse;
import static org.junit.jupiter.api.Assertions.assertTrue;

import java.time.Clock;
import java.time.Duration;
import java.time.Instant;
import java.time.ZoneId;
import java.time.ZoneOffset;
import org.junit.jupiter.api.Test;

class LoginThrottleTest {

    private static final String KEY = "admin|127.0.0.1";

    @Test
    void blocksAfterLimitAndRecoversAfterWindow() {
        var clock = new TickClock(Instant.parse("2026-01-01T00:00:00Z"));
        var throttle = new LoginThrottle(2, 60, clock);

        assertTrue(throttle.allowed(KEY));
        throttle.recordFailure(KEY);
        assertTrue(throttle.allowed(KEY));
        throttle.recordFailure(KEY);
        assertFalse(throttle.allowed(KEY));

        clock.advance(Duration.ofSeconds(60));
        assertTrue(throttle.allowed(KEY));
    }

    @Test
    void pruneDropsExpiredWindows() {
        var clock = new TickClock(Instant.parse("2026-01-01T00:00:00Z"));
        var throttle = new LoginThrottle(5, 60, clock);

        throttle.recordFailure("admin|10.0.0.1");
        throttle.recordFailure("admin|10.0.0.2");
        assertEquals(2, throttle.size());

        clock.advance(Duration.ofSeconds(61));
        throttle.prune();

        assertEquals(0, throttle.size());
    }

    @Test
    void mapStaysBoundedOnUsernameFlood() {
        var clock = new TickClock(Instant.parse("2026-01-01T00:00:00Z"));
        var throttle = new LoginThrottle(5, 300, clock);

        // Перебор уникальных логинов с одного IP: без предела карта росла бы
        // линейно по числу попыток и в итоге съела бы heap.
        for (int i = 0; i < LoginThrottle.MAX_KEYS + 100; i++) {
            throttle.recordFailure("user" + i + "|10.0.0.1");
        }

        assertTrue(
            throttle.size() <= LoginThrottle.MAX_KEYS,
            "карта превысила предел: " + throttle.size()
        );
    }

    /** Управляемые часы: тест не зависит от реального времени. */
    private static final class TickClock extends Clock {

        private Instant now;

        TickClock(Instant start) {
            this.now = start;
        }

        void advance(Duration duration) {
            now = now.plus(duration);
        }

        @Override
        public Instant instant() {
            return now;
        }

        @Override
        public ZoneId getZone() {
            return ZoneOffset.UTC;
        }

        @Override
        public Clock withZone(ZoneId zone) {
            return this;
        }
    }
}
