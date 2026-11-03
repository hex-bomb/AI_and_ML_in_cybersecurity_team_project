package ru.vektor.docs;

import io.micrometer.core.instrument.MeterRegistry;
import io.micrometer.core.instrument.Timer;
import jakarta.servlet.Filter;
import jakarta.servlet.FilterChain;
import jakarta.servlet.ServletException;
import jakarta.servlet.ServletRequest;
import jakarta.servlet.ServletResponse;
import jakarta.servlet.http.HttpServletRequest;
import jakarta.servlet.http.HttpServletResponse;
import java.io.IOException;
import java.util.UUID;
import java.util.concurrent.TimeUnit;
import org.springframework.core.annotation.Order;
import org.springframework.stereotype.Component;
import org.springframework.web.servlet.HandlerMapping;

@Component
@Order(1)
public class ObsFilter implements Filter {

    private final MeterRegistry meters;

    public ObsFilter(MeterRegistry meters) {
        this.meters = meters;
    }

    @Override
    public void doFilter(
        ServletRequest request,
        ServletResponse response,
        FilterChain chain
    ) throws IOException, ServletException {
        var http = (HttpServletRequest) request;
        var rid = requestId(http);
        request.setAttribute("rid", rid);

        long started = System.nanoTime();
        try {
            chain.doFilter(request, response);
        } finally {
            var status = String.valueOf(
                ((HttpServletResponse) response).getStatus()
            );
            // Шаблон маршрута, а не сырой URI: иначе /documents/{externalId}/source
            // создаёт по таймсерии на документ и взрывает кардинальность Prometheus.
            var pattern = http.getAttribute(
                HandlerMapping.BEST_MATCHING_PATTERN_ATTRIBUTE
            );
            var path = pattern != null ? pattern.toString() : "__unmatched__";
            meters
                .counter(
                    "http.requests",
                    "method",
                    http.getMethod(),
                    "path",
                    path,
                    "status",
                    status
                )
                .increment();
            Timer.builder("http.request.duration")
                .tag("path", path)
                .register(meters)
                .record(System.nanoTime() - started, TimeUnit.NANOSECONDS);
            ((HttpServletResponse) response).setHeader("X-Request-ID", rid);
        }
    }

    private static String requestId(HttpServletRequest request) {
        var incoming = request.getHeader("X-Request-ID");
        if (
            incoming != null && !incoming.isBlank() && incoming.length() <= 128
        ) {
            return incoming;
        }
        return UUID.randomUUID().toString().replace("-", "");
    }
}
