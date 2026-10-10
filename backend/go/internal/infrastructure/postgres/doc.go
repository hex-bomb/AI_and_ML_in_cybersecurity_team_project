// Package postgres — адаптеры портов домена к PostgreSQL (договор типов:
// UUID — string на границе; время — UTC по ports.Clock; jsonb — UseNumber;
// NULL — в указатель; пустой срез — не NULL).
package postgres
