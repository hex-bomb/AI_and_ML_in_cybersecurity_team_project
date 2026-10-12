package indexing

import (
	"context"
	"encoding/json"

	"github.com/kbsearch/kb/internal/envelope"
	"github.com/kbsearch/kb/internal/ingest/parser"
	"github.com/kbsearch/kb/internal/ports"
)

// keyBuilder строит ключ файла в хранилище.
//
// Передаётся снаружи, а не собирается здесь: правило именования живёт в
// infrastructure/storage, и второй его экземпляр в слое приложения
// разошёлся бы с первым при первой же правке формата ключа. Приложение не
// импортирует инфраструктуру, поэтому единственный способ не дублировать —
// получить функцию при сборке. Ровно так же уже сделан ключ оригинала.
type keyBuilder func(tenantID, versionID string) string

// Converter runs the convert stage: original bytes in, ParsedDocument on disk.
type Converter struct {
	deps Deps
}

// NewConverter assembles the convert stage.
func NewConverter(deps Deps) *Converter { return &Converter{deps: deps} }

// Convert processes one convert-stage job.
//
// Порядок шагов: разбор, запись файла, транзакция. Файл пишется ДО транзакции
// сознательно: транзакция не откатывает файловое хранилище, и откат
// parsed_key при уже записанном файле безвреден — повтор перезапишет его тем
// же содержимым, потому что разбор детерминирован. Обратный порядок оставил
// бы версию в статусе parsed без parsed.json, и стадия index падала бы навсегда.
func (c *Converter) Convert(ctx context.Context, lease domain.JobLease) error {
	versionID, ok := lease.VersionIDValue()
	if !ok {
		return domain.Conflict("Задание стадии convert не ссылается на версию документа")
	}

	tenantID := lease.TenantID.String()
	version := versionID.String()

	parsedKey, err := c.parsedKey(tenantID, version)
	if err != nil {
		return err
	}

	source, err := c.deps.Documents.LoadVersion(ctx, c.deps.Queryer, c.deps.Storage, tenantID, version)
	if err != nil {
		return err
	}

	parsed, err := documents.Convert(ctx, source.Content, documents.ConversionRequest{
		TenantID:   lease.TenantID,
		KBID:       lease.KBID,
		DocumentID: lease.DocumentID,
		VersionID:  versionID,
		Title:      source.Title,
		Filename:   source.Filename,
		SourceURL:  source.SourceURL,
		Metadata:   source.Metadata,
	})
	if err != nil {
		return err
	}

	encoded, err := json.Marshal(parsed)
	if err != nil {
		// json.Marshal падает на значении из metadata — там лежит то, что
		// прислал клиент. Повтор не поможет, поэтому наружу уходит код
		// домена, а причина остаётся в Cause для журнала.
		return domain.Internal("Не удалось закодировать разобранный документ").WithCause(err)
	}

	if err := c.deps.Storage.Put(ctx, parsedKey, encoded); err != nil {
		return err
	}

	return c.advance(ctx, lease, domain.ConversionResult{
		VersionID:  version,
		ParsedKey:  parsedKey,
		BlockCount: len(parsed.Blocks),
	})
}

// parsedKey строит ключ parsed.json.
//
// Без keyBuilder сборка неполна, и подставлять запасной формат нельзя: ключ
// должен совпадать с тем, по которому инфраструктура его записывает и читает.
// Молча уйти в другой путь означало бы записать parsed.json туда, откуда его
// никто не прочитает, — навсегда зависшую версию при «успешной» стадии.
func (c *Converter) parsedKey(tenantID, versionID string) (string, error) {
	if c.deps.ParsedKey == nil {
		return "", domain.ConfigurationInvalid(
			"Сборка стадии convert не задала построитель ключа parsed.json",
			map[string]any{"tenant_id": tenantID, "version_id": versionID},
		)
	}
	return c.deps.ParsedKey(tenantID, versionID), nil
}

// advance переводит задание на стадию index.
//
// Запись parsed_key, статуса версии и стадии задания идёт одним вызовом
// FinishConvert в одной транзакции. Разносить её по репозиториям нельзя:
// версия со статусом parsed и задание, всё ещё висящее на convert, разошлись
// бы при любом сбое между двумя UPDATE, и документ завис бы навсегда —
// повтор стадии convert не поставил бы index, потому что parsed_key уже записан.
func (c *Converter) advance(
	ctx context.Context, lease domain.JobLease, result domain.ConversionResult,
) error {
	return c.deps.Transactor.WithTx(ctx, func(tx ports.Queryer) error {
		return c.deps.Jobs.FinishConvert(
			ctx, tx, lease.JobID.String(), lease.LeaseToken, result)
	})
}
