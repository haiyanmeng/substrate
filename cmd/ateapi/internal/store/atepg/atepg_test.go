// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package atepg

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"

	"go.ate.dev/substrate/api/ateapi/v1"
	"go.ate.dev/substrate/cmd/ateapi/internal/store"
	"go.ate.dev/substrate/internal/resources"
)

// A listing fails as a whole on one undecodable row, so the error has to name
// the row.
func TestList_NamesAnUndecodableRow(t *testing.T) {
	s := setupPostgresPersistence(t)
	ctx := context.Background()
	createTestAtespace(t, s, "team-a")
	createTestActorTemplate(t, s, "team-a", "t1")
	createTestTag(t, s, "team-a", "v1")
	if _, err := s.CreateActor(ctx, &ateapi.Actor{
		Metadata: &ateapi.ResourceMetadata{Name: "a1", Atespace: "team-a"},
		Status:   &ateapi.ActorStatus{State: ateapi.ActorState_ACTOR_STATE_SUSPENDED},
	}); err != nil {
		t.Fatalf("CreateActor failed: %v", err)
	}
	if _, err := s.CreateWorker(ctx, &ateapi.Worker{
		Metadata:        &ateapi.ResourceMetadata{Name: "w1"},
		WorkerNamespace: "ns",
		WorkerPool:      "pool",
		WorkerPod:       "pod",
	}); err != nil {
		t.Fatalf("CreateWorker failed: %v", err)
	}

	// Not a valid encoding of any message.
	bad := []byte{0xff}
	for _, table := range []string{"atespaces", "actors", "actor_templates", "tags", "workers"} {
		if _, err := s.pool.Exec(ctx, "UPDATE "+table+" SET proto = $1", bad); err != nil {
			t.Fatalf("corrupting %s: %v", table, err)
		}
	}
	if _, err := s.pool.Exec(ctx, `INSERT INTO worker_assignments (actor_uid, worker_name, proto) VALUES ('uid-1', 'w1', $1)`, bad); err != nil {
		t.Fatalf("inserting assignment: %v", err)
	}

	opts := store.ListOptions{PageSize: 10}
	tests := []struct {
		name string
		list func() error
		want string
	}{
		{"atespaces", func() error { _, err := s.ListAtespaces(ctx, opts); return err }, "atespace team-a"},
		{"actors", func() error { _, err := s.ListActors(ctx, "team-a", opts); return err }, "actor team-a/a1"},
		{"actors, all atespaces", func() error { _, err := s.ListActors(ctx, "", opts); return err }, "actor team-a/a1"},
		{"actor templates", func() error { _, err := s.ListActorTemplates(ctx, "team-a", opts); return err }, "actor template team-a/t1"},
		{"actor templates, all atespaces", func() error { _, err := s.ListActorTemplates(ctx, "", opts); return err }, "actor template team-a/t1"},
		{"tags", func() error { _, err := s.ListTags(ctx, "team-a", opts); return err }, "tag team-a/v1"},
		{"tags, all atespaces", func() error { _, err := s.ListTags(ctx, "", opts); return err }, "tag team-a/v1"},
		{"workers", func() error { _, err := s.ListWorkers(ctx, opts); return err }, "worker w1"},
		{"worker assignments", func() error { _, err := s.ListWorkerAssignments(ctx, "w1", opts); return err }, "actor uid-1"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.list()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("list error = %v, want one naming %q", err, tc.want)
			}
		})
	}
}

// afterStatement is a pgx tracer that runs fn once, after the first statement
// on table ends.
type afterStatement struct {
	mu    sync.Mutex
	table string
	fn    func()
}

type afterStatementKey struct{}

func (a *afterStatement) arm(table string, fn func()) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.table, a.fn = table, fn
}

func (a *afterStatement) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.fn == nil || !strings.Contains(normalizeSQL(data.SQL)+" ", " "+a.table+" ") {
		return ctx
	}
	fn := a.fn
	a.fn = nil
	return context.WithValue(ctx, afterStatementKey{}, fn)
}

func (a *afterStatement) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryEndData) {
	if fn, ok := ctx.Value(afterStatementKey{}).(func()); ok {
		fn()
	}
}

// An unconditional delete of a missing row reports ErrNotFound even if the
// row is created right after the delete first reads its table.
func TestDelete_MissingRowCreatedConcurrently(t *testing.T) {
	s := setupPostgresPersistence(t)
	ctx := t.Context()
	createTestAtespace(t, s, "team-a")
	actorRef := resources.ActorRef{Atespace: "team-a", Name: "a1"}
	if _, err := s.CreateActor(ctx, &ateapi.Actor{
		Metadata: &ateapi.ResourceMetadata{Atespace: actorRef.Atespace, Name: actorRef.Name},
		Status:   &ateapi.ActorStatus{State: ateapi.ActorState_ACTOR_STATE_SUSPENDED},
	}); err != nil {
		t.Fatalf("CreateActor failed: %v", err)
	}
	hook := &afterStatement{}
	traced, err := NewPersistence(ctx, openPool(t, "public", hook))
	if err != nil {
		t.Fatalf("NewPersistence failed: %v", err)
	}
	t.Cleanup(traced.Close)
	traced.SetPolicyManager(s.policyManager)

	tagRef := resources.TagRef{Atespace: "team-a", Name: "v1"}
	templateRef := resources.ActorTemplateRef{Atespace: "team-a", Name: "t1"}
	var none store.DeletePreconditions
	tests := []struct {
		name   string
		table  string
		create func(t *testing.T)
		del    func() error
		get    func() error
	}{
		{
			name:   "tag",
			table:  "tags",
			create: func(t *testing.T) { createTestTag(t, s, tagRef.Atespace, tagRef.Name) },
			del:    func() error { _, err := traced.DeleteTag(ctx, tagRef, none); return err },
			get:    func() error { _, err := s.GetTag(ctx, tagRef); return err },
		},
		{
			name:   "actor template",
			table:  "actor_templates",
			create: func(t *testing.T) { createTestActorTemplate(t, s, templateRef.Atespace, templateRef.Name) },
			del:    func() error { _, err := traced.DeleteActorTemplate(ctx, templateRef, none); return err },
			get:    func() error { _, err := s.GetActorTemplate(ctx, templateRef); return err },
		},
		{
			name:  "egress policy",
			table: "actor_egress_policies",
			create: func(t *testing.T) {
				if _, err := s.CreateEgressPolicy(ctx, actorRef, &ateapi.EgressPolicy{
					Metadata: &ateapi.ResourceMetadata{Atespace: actorRef.Atespace, Name: "default"},
				}); err != nil {
					t.Fatalf("CreateEgressPolicy failed: %v", err)
				}
			},
			del: func() error { _, err := traced.DeleteEgressPolicy(ctx, actorRef, none); return err },
			get: func() error { _, err := s.GetEgressPolicy(ctx, actorRef); return err },
		},
		{
			name:   "atespace",
			table:  "atespaces",
			create: func(t *testing.T) { createTestAtespace(t, s, "team-b") },
			del:    func() error { _, err := traced.DeleteAtespace(ctx, "team-b", none); return err },
			get:    func() error { _, err := s.GetAtespace(ctx, "team-b"); return err },
		},
		{
			name:  "atespace access policy",
			table: "atespace_access_policies",
			create: func(t *testing.T) {
				if _, err := s.CreateAtespaceAccessPolicy(ctx, "team-a", &ateapi.AccessPolicy{
					Metadata: &ateapi.ResourceMetadata{Name: "default"},
				}); err != nil {
					t.Fatalf("CreateAtespaceAccessPolicy failed: %v", err)
				}
			},
			del: func() error { _, err := traced.DeleteAtespaceAccessPolicy(ctx, "team-a", none); return err },
			get: func() error { _, err := s.GetAtespaceAccessPolicy(ctx, "team-a"); return err },
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			hook.arm(tc.table, func() { tc.create(t) })
			if err := tc.del(); !errors.Is(err, store.ErrNotFound) {
				t.Errorf("delete = %v, want ErrNotFound", err)
			}
			if err := tc.get(); err != nil {
				t.Errorf("get after the delete = %v, want the concurrently created row", err)
			}
		})
	}
}
