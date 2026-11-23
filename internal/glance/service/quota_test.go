package service

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/PaoloCastanon/modules-devstack-optimized/internal/glance/model"
	"github.com/PaoloCastanon/modules-devstack-optimized/internal/glance/quota"
)

type quotaFake struct {
	err   error
	calls int
}

func (q *quotaFake) EnforceImageCount(context.Context, string) error { q.calls++; return q.err }
func (q *quotaFake) EnforceUpload(context.Context, string) error     { q.calls++; return q.err }
func (q *quotaFake) EnforceStage(context.Context, string) error      { q.calls++; return q.err }
func (q *quotaFake) Usage(context.Context, string) (map[string]quota.Limit, error) {
	return map[string]quota.Limit{quota.ImageCountTotal: {Limit: 100, Usage: 1}}, nil
}

func TestCreateEnforcesImageCountTotal(t *testing.T) {
	repo := &repositoryStub{}
	s := New(repo, time.Now)
	ctx := context.Background()
	if usage, err := s.Usage(ctx, identity("reader")); err != nil || len(usage) != 0 {
		t.Fatal("usage without Keystone limits must be empty", usage, err)
	}
	q := &quotaFake{err: model.Fail(model.LimitExceeded, "over")}
	s.SetQuota(q)
	if _, err := s.Create(ctx, identity("member"), model.CreateInput{}); domainKind(err) != model.LimitExceeded || repo.creates != 0 {
		t.Fatal("image created over the limit", err, repo.creates)
	}
	q.err = nil
	if _, err := s.Create(ctx, identity("member"), model.CreateInput{}); err != nil || repo.creates != 1 || q.calls != 2 {
		t.Fatal(err, repo.creates, q.calls)
	}
	if usage, err := s.Usage(ctx, identity("reader")); err != nil || usage[quota.ImageCountTotal].Limit != 100 {
		t.Fatal(usage, err)
	}
}

func TestUploadEnforcesLimitsBeforeReserving(t *testing.T) {
	u, d, _, log := uploadFixture(t)
	q := &quotaFake{err: model.Fail(model.LimitExceeded, "over")}
	u.SetQuota(q)
	size := int64(len(uploadPayload))
	if _, err := u.Upload(context.Background(), identity("member"), uploadImageID, bytes.NewReader(uploadPayload), &size); domainKind(err) != model.LimitExceeded {
		t.Fatal(err)
	}
	for _, op := range log.snapshot() {
		if op == "sql.Reserve" {
			t.Fatal("reserved over the limit")
		}
	}
	if d.image.Status != "queued" {
		t.Fatal(d.image.Status)
	}
}
