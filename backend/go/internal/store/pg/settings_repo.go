package postgres

import (
	"bytes"
	"context"
	"encoding/json"

	"github.com/google/uuid"

	domain "github.com/kbsearch/kb/internal/envelope"
	"github.com/kbsearch/kb/internal/ports"
)

// SettingsRepository — реализация ports.SettingsRepository (только чтение).
type SettingsRepository struct{}

var _ ports.SettingsRepository = (*SettingsRepository)(nil)

// NewSettingsRepository создаёт репозиторий настроек.
func NewSettingsRepository() *SettingsRepository { return &SettingsRepository{} }

// Levels возвращает значения уровней KB и tenant раздельно (приоритет — у сценария).
func (r *SettingsRepository) Levels(
	ctx context.Context, q ports.Queryer, tenantID, kbID string,
) (map[string]any, map[string]any, error) {
	tenant, err := parseUUID("tenant_id", tenantID)
	if err != nil {
		return nil, nil, err
	}

	args := []any{tenant}
	condition := "kb_id IS NULL"
	if kbID != "" {
		kb, err := parseUUID("kb_id", kbID)
		if err != nil {
			return nil, nil, err
		}
		condition = "(kb_id = $2 OR kb_id IS NULL)"
		args = append(args, kb)
	}

	// Условие зависит от наличия kbID, поэтому не константа.
	query := `
		SELECT kb_id, key, value
		FROM settings
		WHERE tenant_id = $1 AND ` + condition

	rows, err := q.Query(ctx, query, args...)
	if err != nil {
		return nil, nil, err
	}

	kbLevel := map[string]any{}
	tenantLevel := map[string]any{}
	err = collect(rows, func() error {
		var (
			rowKBID *uuid.UUID
			key     string
			raw     []byte
		)
		if err := rows.Scan(&rowKBID, &key, &raw); err != nil {
			return err
		}
		value, err := decodeSettingValue(raw)
		if err != nil {
			return err
		}
		if rowKBID == nil {
			tenantLevel[key] = value
			return nil
		}
		kbLevel[key] = value
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	return kbLevel, tenantLevel, nil
}

// decodeSettingValue разбирает JSONB через UseNumber (целые — int64, как Python json.loads).
func decodeSettingValue(raw []byte) (any, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()

	var decoded any
	if err := decoder.Decode(&decoded); err != nil {
		return nil, (&domain.AppError{
			Code:    domain.CodeConfigurationInvalid,
			Status:  409,
			Message: "Значение настройки не является корректным JSON",
			Cause:   err,
		})
	}
	return normalizeJSONNumbers(decoded), nil
}

// normalizeJSONNumbers превращает json.Number в int64 либо float64.
func normalizeJSONNumbers(value any) any {
	switch typed := value.(type) {
	case json.Number:
		if integer, err := typed.Int64(); err == nil {
			return integer
		}
		if float, err := typed.Float64(); err == nil {
			return float
		}
		return typed
	case []any:
		out := make([]any, len(typed))
		for i, item := range typed {
			out[i] = normalizeJSONNumbers(item)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(typed))
		for key, item := range typed {
			out[key] = normalizeJSONNumbers(item)
		}
		return out
	default:
		return value
	}
}
