package app

import (
	"context"

	"github.com/google/uuid"

	httpapi "github.com/kbsearch/kb/internal/api/http"
	domain "github.com/kbsearch/kb/internal/envelope"
	documents "github.com/kbsearch/kb/internal/ingest/parser"
)

// documentAdapter переводит DTO транспорта в типы сценария документов.

// documentView переводит представление сценария в представление ответа.
func documentView(v documents.View) httpapi.DocumentView {
	return httpapi.DocumentView{
		ID:        v.ID,
		KBID:      v.KBID,
		Filename:  v.Filename,
		Title:     v.Title,
		VersionID: v.VersionID,
		State:     v.State,
		Stage:     v.Stage,
		CreatedAt: v.CreatedAt,
		UpdatedAt: v.UpdatedAt,
		Chunks:    v.Chunks,
		SourceURL: v.SourceURL,
		ACL:       v.ACL,
	}
}

type documentAdapter struct{ svc *documents.Service }

func (a documentAdapter) Upload(
	ctx context.Context, p domain.Principal, in httpapi.UploadInput,
) (httpapi.UploadResult, error) {
	created, err := a.svc.Upload(ctx, p, documents.UploadInput{
		KBID:           in.KBID,
		Filename:       in.Filename,
		Title:          in.Title,
		Content:        in.Content,
		SourceURL:      in.SourceURL,
		Metadata:       in.Metadata,
		IdempotencyKey: in.IdempotencyKey,
	})
	if err != nil {
		return httpapi.UploadResult{}, err
	}
	return httpapi.UploadResult{
		DocumentID: created.DocumentID,
		VersionID:  created.VersionID,
		JobID:      created.JobID,
	}, nil
}

func (a documentAdapter) List(
	ctx context.Context, p domain.Principal, in httpapi.ListInput,
) (httpapi.ListOutput, error) {
	out, err := a.svc.List(ctx, p, documents.ListInput{
		KBID: in.KBID, Limit: in.Limit, Cursor: in.Cursor,
	})
	if err != nil {
		return httpapi.ListOutput{}, err
	}
	items := make([]httpapi.DocumentView, 0, len(out.Items))
	for _, v := range out.Items {
		items = append(items, documentView(v))
	}
	return httpapi.ListOutput{Items: items, NextCursor: out.NextCursor}, nil
}

func (a documentAdapter) Get(
	ctx context.Context, p domain.Principal, id uuid.UUID,
) (httpapi.DocumentView, error) {
	v, err := a.svc.Get(ctx, p, id)
	if err != nil {
		return httpapi.DocumentView{}, err
	}
	return documentView(v), nil
}

func (a documentAdapter) Delete(
	ctx context.Context, p domain.Principal, id uuid.UUID,
) error {
	return a.svc.Delete(ctx, p, id)
}

func (a documentAdapter) ReplaceContent(
	ctx context.Context, p domain.Principal, in httpapi.ReplaceContentInput,
) (uuid.UUID, error) {
	return a.svc.ReplaceContent(ctx, p, documents.ReplaceContentInput{
		DocumentID:     in.DocumentID,
		Filename:       in.Filename,
		Title:          in.Title,
		Content:        in.Content,
		Metadata:       in.Metadata,
		IdempotencyKey: in.IdempotencyKey,
	})
}

func (a documentAdapter) Reindex(
	ctx context.Context, p domain.Principal, id uuid.UUID,
) (uuid.UUID, error) {
	return a.svc.Reindex(ctx, p, id)
}

func (a documentAdapter) Retry(
	ctx context.Context, p domain.Principal, id uuid.UUID,
) (uuid.UUID, error) {
	return a.svc.Retry(ctx, p, id)
}

func (a documentAdapter) SetACL(
	ctx context.Context, p domain.Principal, id uuid.UUID, acl domain.AllowedList,
) error {
	return a.svc.SetACL(ctx, p, id, acl)
}

func (a documentAdapter) Source(
	ctx context.Context, p domain.Principal, id uuid.UUID,
) ([]byte, string, error) {
	return a.svc.Source(ctx, p, id)
}
