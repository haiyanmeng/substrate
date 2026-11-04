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
	"fmt"
	"strings"
	"testing"
	"time"

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

// Updates hold the row locked from read to write. A write started from inside
// mutate waits for the update to commit and then acts on its result.
func TestUpdate_ConcurrentWriteWaitsForRowLock(t *testing.T) {
	actorRef := resources.ActorRef{Atespace: "team-a", Name: "a1"}
	// policyRef names an actor every subtest creates to hold the egress policy.
	policyRef := resources.ActorRef{Atespace: "team-a", Name: "a2"}
	templateRef := resources.ActorTemplateRef{Atespace: "team-a", Name: "t1"}
	tagRef := resources.TagRef{Atespace: "team-a", Name: "v1"}
	newActor := func(ref resources.ActorRef) *ateapi.Actor {
		return &ateapi.Actor{
			Metadata: &ateapi.ResourceMetadata{Atespace: ref.Atespace, Name: ref.Name},
			Status:   &ateapi.ActorStatus{State: ateapi.ActorState_ACTOR_STATE_SUSPENDED},
		}
	}
	newPolicy := func() *ateapi.EgressPolicy {
		return &ateapi.EgressPolicy{Metadata: &ateapi.ResourceMetadata{Atespace: policyRef.Atespace, Name: "default"}}
	}
	type resource struct {
		name string
		// table is matched against the waiting backend's query.
		table  string
		create func(context.Context, *Persistence) (*ateapi.ResourceMetadata, error)
		get    func(context.Context, *Persistence) (*ateapi.ResourceMetadata, error)
		update func(context.Context, *Persistence, store.Precondition, func() error) (*ateapi.ResourceMetadata, error)
		delete func(context.Context, *Persistence) error
	}
	resourceTypes := []resource{
		{
			name:  "actor",
			table: "actors",
			create: func(ctx context.Context, s *Persistence) (*ateapi.ResourceMetadata, error) {
				a, err := s.CreateActor(ctx, newActor(actorRef))
				return a.GetMetadata(), err
			},
			get: func(ctx context.Context, s *Persistence) (*ateapi.ResourceMetadata, error) {
				a, err := s.GetActor(ctx, actorRef)
				return a.GetMetadata(), err
			},
			update: func(ctx context.Context, s *Persistence, pre store.Precondition, mutate func() error) (*ateapi.ResourceMetadata, error) {
				a, err := s.UpdateActor(ctx, actorRef, pre, func(*ateapi.Actor) error { return mutate() })
				return a.GetMetadata(), err
			},
			delete: func(ctx context.Context, s *Persistence) error {
				_, err := s.DeleteActor(ctx, actorRef, store.DeletePreconditions{})
				return err
			},
		},
		{
			name:  "actor template",
			table: "actor_templates",
			create: func(ctx context.Context, s *Persistence) (*ateapi.ResourceMetadata, error) {
				tmpl, err := s.CreateActorTemplate(ctx, &ateapi.ActorTemplate{
					Metadata: &ateapi.ResourceMetadata{Atespace: templateRef.Atespace, Name: templateRef.Name},
				})
				return tmpl.GetMetadata(), err
			},
			get: func(ctx context.Context, s *Persistence) (*ateapi.ResourceMetadata, error) {
				tmpl, err := s.GetActorTemplate(ctx, templateRef)
				return tmpl.GetMetadata(), err
			},
			update: func(ctx context.Context, s *Persistence, pre store.Precondition, mutate func() error) (*ateapi.ResourceMetadata, error) {
				tmpl, err := s.UpdateActorTemplate(ctx, templateRef, pre, func(*ateapi.ActorTemplate) error { return mutate() })
				return tmpl.GetMetadata(), err
			},
			delete: func(ctx context.Context, s *Persistence) error {
				_, err := s.DeleteActorTemplate(ctx, templateRef, store.DeletePreconditions{})
				return err
			},
		},
		{
			name:  "tag",
			table: "tags",
			create: func(ctx context.Context, s *Persistence) (*ateapi.ResourceMetadata, error) {
				tag, err := s.CreateTag(ctx, newTestTag(tagRef.Atespace, tagRef.Name))
				return tag.GetMetadata(), err
			},
			get: func(ctx context.Context, s *Persistence) (*ateapi.ResourceMetadata, error) {
				tag, err := s.GetTag(ctx, tagRef)
				return tag.GetMetadata(), err
			},
			update: func(ctx context.Context, s *Persistence, pre store.Precondition, mutate func() error) (*ateapi.ResourceMetadata, error) {
				tag, err := s.UpdateTag(ctx, tagRef, pre, func(*ateapi.Tag) error { return mutate() })
				return tag.GetMetadata(), err
			},
			delete: func(ctx context.Context, s *Persistence) error {
				_, err := s.DeleteTag(ctx, tagRef, store.DeletePreconditions{})
				return err
			},
		},
		{
			name:  "egress policy",
			table: "actor_egress_policies",
			create: func(ctx context.Context, s *Persistence) (*ateapi.ResourceMetadata, error) {
				p, err := s.CreateEgressPolicy(ctx, policyRef, newPolicy())
				return p.GetMetadata(), err
			},
			get: func(ctx context.Context, s *Persistence) (*ateapi.ResourceMetadata, error) {
				p, err := s.GetEgressPolicy(ctx, policyRef)
				return p.GetMetadata(), err
			},
			update: func(ctx context.Context, s *Persistence, pre store.Precondition, mutate func() error) (*ateapi.ResourceMetadata, error) {
				p, err := s.UpdateEgressPolicy(ctx, policyRef, pre, func(*ateapi.EgressPolicy) error { return mutate() })
				return p.GetMetadata(), err
			},
			delete: func(ctx context.Context, s *Persistence) error {
				_, err := s.DeleteEgressPolicy(ctx, policyRef, store.DeletePreconditions{})
				return err
			},
		},
	}
	writes := []struct {
		name    string
		write   func(context.Context, *Persistence, resource, store.Precondition) error
		wantErr error
		// check inspects the row after both writes, given the update's result.
		check func(t *testing.T, updated, stored *ateapi.ResourceMetadata, getErr error)
	}{
		{
			name: "update",
			write: func(ctx context.Context, s *Persistence, r resource, pre store.Precondition) error {
				_, err := r.update(ctx, s, pre, func() error { return nil })
				return err
			},
			wantErr: store.ErrVersionConflict,
			check: func(t *testing.T, updated, stored *ateapi.ResourceMetadata, getErr error) {
				if getErr != nil || stored.GetVersion() != updated.GetVersion() {
					t.Errorf("stored version = %d, %v; want %d", stored.GetVersion(), getErr, updated.GetVersion())
				}
			},
		},
		{
			name: "delete",
			write: func(ctx context.Context, s *Persistence, r resource, _ store.Precondition) error {
				return r.delete(ctx, s)
			},
			check: func(t *testing.T, _, _ *ateapi.ResourceMetadata, getErr error) {
				if !errors.Is(getErr, store.ErrNotFound) {
					t.Errorf("get after delete = %v, want ErrNotFound", getErr)
				}
			},
		},
		{
			name: "delete and recreate",
			write: func(ctx context.Context, s *Persistence, r resource, _ store.Precondition) error {
				if err := r.delete(ctx, s); err != nil {
					return err
				}
				_, err := r.create(ctx, s)
				return err
			},
			check: func(t *testing.T, updated, stored *ateapi.ResourceMetadata, getErr error) {
				if getErr != nil || stored.GetUid() == updated.GetUid() || stored.GetVersion() != 1 {
					t.Errorf("stored uid %q version %d, %v; want a new uid at version 1", stored.GetUid(), stored.GetVersion(), getErr)
				}
			},
		},
	}
	for _, r := range resourceTypes {
		for _, w := range writes {
			t.Run(r.name+"/"+w.name, func(t *testing.T) {
				s := setupPostgresPersistence(t)
				ctx := context.Background()
				createTestAtespace(t, s, "team-a")
				if _, err := s.CreateActor(ctx, newActor(policyRef)); err != nil {
					t.Fatalf("CreateActor failed: %v", err)
				}
				created, err := r.create(ctx, s)
				if err != nil {
					t.Fatalf("create failed: %v", err)
				}
				pre := store.Precondition{UID: created.GetUid(), Version: created.GetVersion()}

				done := make(chan error, 1)
				updated, err := r.update(ctx, s, pre, func() error {
					go func() { done <- w.write(ctx, s, r, pre) }()
					return waitForLockWait(ctx, s, r.table, done)
				})
				if err != nil {
					t.Fatalf("update failed: %v", err)
				}
				if got, want := updated.GetVersion(), created.GetVersion()+1; got != want {
					t.Errorf("updated version = %d, want %d", got, want)
				}
				if err := <-done; !errors.Is(err, w.wantErr) {
					t.Errorf("concurrent %s = %v, want %v", w.name, err, w.wantErr)
				}
				stored, getErr := r.get(ctx, s)
				w.check(t, updated, stored, getErr)
			})
		}
	}
}

// waitForLockWait returns once a backend whose query names table waits on a
// lock. It fails if done delivers first, as the write it reports did not wait.
func waitForLockWait(ctx context.Context, s *Persistence, table string, done <-chan error) error {
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		var waiting bool
		if err := s.pool.QueryRow(ctx, `
			SELECT EXISTS (SELECT 1 FROM pg_stat_activity
			WHERE wait_event_type = 'Lock' AND query LIKE '%' || $1 || '%')`, table).Scan(&waiting); err != nil {
			return fmt.Errorf("checking for lock waits: %w", err)
		}
		if waiting {
			return nil
		}
		select {
		case err := <-done:
			return fmt.Errorf("concurrent write returned %v without waiting for the row lock", err)
		case <-tick.C:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}
