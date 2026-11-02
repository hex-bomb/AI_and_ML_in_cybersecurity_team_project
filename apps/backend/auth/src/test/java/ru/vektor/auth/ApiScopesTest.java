package ru.vektor.auth;

import static org.junit.jupiter.api.Assertions.assertEquals;

import java.util.List;
import org.junit.jupiter.api.Test;

class ApiScopesTest {

    @Test
    void roleToAudienceMappingIsExplicit() {
        assertEquals(List.of("public"), Api.scopes("user"));
        assertEquals(List.of("public", "employee"), Api.scopes("employee"));
        assertEquals(List.of("public", "employee"), Api.scopes("admin"));
        // Неизвестная роль не должна получать внутренние документы.
        assertEquals(List.of("public"), Api.scopes("someone-else"));
    }
}
