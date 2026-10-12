package app

import (
	"context"

	"github.com/google/uuid"

	httpapi "github.com/kbsearch/kb/internal/api/http"
	"github.com/kbsearch/kb/internal/docs/kb"
	domain "github.com/kbsearch/kb/internal/envelope"
	"github.com/kbsearch/kb/internal/ports"
	"github.com/kbsearch/kb/internal/search/answers"
	search "github.com/kbsearch/kb/internal/search/service"
	postgres "github.com/kbsearch/kb/internal/store/pg"
)

// Адаптеры переводят DTO транспорта в типы сценариев (application не может
// импортировать delivery). Nil-срезы нормализуются в пустые ради JSON [].

// aclScopeAdapter переводит вопрос сценария в вызов оракула прав §6.
//
// Слово «переводит» здесь буквальное: оракул отвечает на вопрос спецификации
// («какие контейнеры и принципалы доступны этому пользователю»), а поиск
// спрашивает о своём («область доступа для этого запроса, с границами тенанта и
// базы»). Меняется форма вызова и ничего больше, поэтому тащить такую обвязку в
// порты ради одного сценария незачем — это работа композиционного корня.
//
// Адаптер — не украшение: search.AccessScopeResolver объявлен потребителем
// (§8), и без реализации в composition root поиск не собрать. Единственное, что
// тут запрещено, — «съесть» ошибку оракула: сбой прав обязан дойти до сценария и
// стать отказом, а не пустой областью (см. Service.resolveScope).
type aclScopeAdapter struct{ resolver *postgres.ACLResolver }

func (a aclScopeAdapter) ResolveScope(
	ctx context.Context, q ports.Queryer, p domain.Principal, kbID string,
) (domain.AccessScope, error) {
	return a.resolver.ResolveScope(ctx, q, postgres.ScopeParams{Principal: p, KBID: kbID})
}

type searchAdapter struct{ svc *search.Service }

func (a searchAdapter) Search(
	ctx context.Context, p domain.Principal, in httpapi.SearchInput,
) (httpapi.SearchOutput, error) {
	out, err := a.svc.Search(ctx, p, search.Request{
		KBID: in.KBID, Query: in.Query, TopK: in.TopK,
	})
	if err != nil {
		return httpapi.SearchOutput{}, err
	}
	hits := out.Hits
	if hits == nil {
		hits = []domain.SearchCandidate{}
	}
	return httpapi.SearchOutput{Status: out.Status, Hits: hits}, nil
}

type answerAdapter struct{ svc *answers.Service }

func (a answerAdapter) Answer(
	ctx context.Context, p domain.Principal, in httpapi.AnswerInput,
) (httpapi.AnswerOutput, error) {
	out, err := a.svc.Answer(ctx, p, answers.Request{
		KBID: in.KBID, Question: in.Question,
	})
	if err != nil {
		return httpapi.AnswerOutput{}, err
	}
	citations := out.Citations
	if citations == nil {
		citations = []domain.Citation{}
	}
	return httpapi.AnswerOutput{
		Status: out.Status, Answer: out.Answer, Citations: citations,
	}, nil
}

type kbAdapter struct{ svc *kb.Service }

func (a kbAdapter) CreateKB(
	ctx context.Context, p domain.Principal, name string,
) (uuid.UUID, error) {
	return a.svc.CreateKB(ctx, p, name)
}

func (a kbAdapter) ListKBs(
	ctx context.Context, p domain.Principal, limit int, cursor string,
) ([]httpapi.KBView, string, error) {
	items, next, err := a.svc.ListKBs(ctx, p, limit, cursor)
	if err != nil {
		return nil, "", err
	}
	views := make([]httpapi.KBView, 0, len(items))
	for _, item := range items {
		views = append(views, httpapi.KBView{
			ID:        item.ID,
			Name:      item.Name,
			CreatedAt: item.CreatedAt,
			ACL:       item.ACL,
		})
	}
	return views, next, nil
}

func (a kbAdapter) GetKB(
	ctx context.Context, p domain.Principal, kbID uuid.UUID,
) (httpapi.KBView, error) {
	item, err := a.svc.GetKB(ctx, p, kbID)
	if err != nil {
		return httpapi.KBView{}, err
	}
	return httpapi.KBView{
		ID: item.ID, Name: item.Name, CreatedAt: item.CreatedAt, ACL: item.ACL,
	}, nil
}

func (a kbAdapter) SetKBACL(
	ctx context.Context, p domain.Principal, kbID uuid.UUID, acl domain.AllowedList,
) error {
	return a.svc.SetKBACL(ctx, p, kbID, acl)
}
