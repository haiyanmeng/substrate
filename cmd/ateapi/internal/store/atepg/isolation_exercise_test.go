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

// The TestExerciseIsolation tests reproduce isolation bugs in the store. Each
// test passes when it reproduces its bug and logs "REPRODUCED".
//
// They need Docker, and start a postgres:18-alpine testcontainer. Run them
// from the repository root:
//
//	go test -race -v -run 'TestExerciseIsolation' ./cmd/ateapi/internal/store/atepg
//
// Narrow -run to one test, e.g. -run 'TestExerciseIsolation_1_'. Without
// Docker the tests skip. Set REQUIRE_DOCKER=true to make them fail instead.

package atepg

import (
	"context"
	"errors"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.ate.dev/substrate/api/ateapi/v1"
	"go.ate.dev/substrate/cmd/ateapi/internal/authz"
	"go.ate.dev/substrate/cmd/ateapi/internal/store"
	"go.ate.dev/substrate/cmd/ateapi/internal/workercache"
	"go.ate.dev/substrate/internal/resources"
)

type sqlHookKey struct{}

type sqlHookTracer struct {
	mu      sync.Mutex
	onStart func(ctx context.Context, sql string)
	onEnd   func(ctx context.Context, sql string)
}

func (t *sqlHookTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	sql := strings.Join(strings.Fields(data.SQL), " ")
	ctx = context.WithValue(ctx, sqlHookKey{}, sql)
	t.mu.Lock()
	fn := t.onStart
	t.mu.Unlock()
	if fn != nil {
		fn(ctx, sql)
	}
	return ctx
}

func (t *sqlHookTracer) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryEndData) {
	sql, _ := ctx.Value(sqlHookKey{}).(string)
	t.mu.Lock()
	fn := t.onEnd
	t.mu.Unlock()
	if fn != nil {
		fn(ctx, sql)
	}
}

func isoOpenPool(t *testing.T, schema string, tracer pgx.QueryTracer, isoLevel string) *pgxpool.Pool {
	t.Helper()
	requirePool(t)
	cfg, err := pgxpool.ParseConfig(containerDSN)
	if err != nil {
		t.Fatalf("ParseConfig failed: %v", err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = pgx.Identifier{schema}.Sanitize()
	if isoLevel != "" {
		cfg.ConnConfig.RuntimeParams["default_transaction_isolation"] = isoLevel
	}
	cfg.ConnConfig.Tracer = tracer
	cfg.MaxConns = 32
	pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
	if err != nil {
		t.Fatalf("NewWithConfig failed: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func isoMigratedPool(t *testing.T, schema string) *pgxpool.Pool {
	t.Helper()
	admin := requirePool(t)
	quoted := pgx.Identifier{schema}.Sanitize()
	if _, err := admin.Exec(t.Context(), `DROP SCHEMA IF EXISTS `+quoted+` CASCADE; CREATE SCHEMA `+quoted); err != nil {
		t.Fatalf("recreating schema %s: %v", schema, err)
	}
	t.Cleanup(func() { _, _ = admin.Exec(context.Background(), `DROP SCHEMA IF EXISTS `+quoted+` CASCADE`) })
	pool := isoOpenPool(t, schema, nil, "")
	p, err := NewPersistence(t.Context(), pool)
	if err != nil {
		t.Fatalf("NewPersistence(%s) failed: %v", schema, err)
	}
	p.Close()
	return pool
}

func isoSetupPersistence(t *testing.T, schema string) *Persistence {
	t.Helper()
	pool := isoMigratedPool(t, schema)
	p, err := NewPersistence(t.Context(), pool)
	if err != nil {
		t.Fatalf("NewPersistence failed: %v", err)
	}
	t.Cleanup(p.Close)
	setTestPolicyManager(t, p)
	return p
}

func isoWaitForLockWaiter(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	for {
		var waiting int
		err := pool.QueryRow(ctx, `
			SELECT count(*) FROM pg_stat_activity
			WHERE datname = current_database()
			  AND state = 'active'
			  AND wait_event_type = 'Lock'`).Scan(&waiting)
		if err != nil {
			if ctx.Err() != nil {
				t.Fatal("timed out waiting for a backend to block on a PostgreSQL lock")
			}
			t.Fatalf("querying pg_stat_activity for lock waiters: %v", err)
		}
		if waiting >= 1 {
			return
		}
		runtime.Gosched()
	}
}

func isoNewWorker(name string) *ateapi.Worker {
	return &ateapi.Worker{
		Metadata:        &ateapi.ResourceMetadata{Name: name},
		WorkerNamespace: "ns",
		WorkerPool:      "pool",
		WorkerPod:       "pod-" + name,
	}
}

func isoNewActor(atespace, name string) *ateapi.Actor {
	return &ateapi.Actor{
		Metadata: &ateapi.ResourceMetadata{Atespace: atespace, Name: name},
		Status:   &ateapi.ActorStatus{State: ateapi.ActorState_ACTOR_STATE_SUSPENDED},
	}
}

func isoNewAssignment(actor *ateapi.Actor) *ateapi.ActorAssignment {
	return &ateapi.ActorAssignment{
		Actor:     &ateapi.ObjectRef{Atespace: actor.GetMetadata().GetAtespace(), Name: actor.GetMetadata().GetName()},
		ActorUid:  actor.GetMetadata().GetUid(),
		Resources: resources.CPUMemory(100, 64<<20),
	}
}

// 1. dropWorkerOutboxPartition reads worker_outbox_trim under READ COMMITTED
// without locking the partition first, so an in-flight writer that commits while
// DROP TABLE waits on its ROW EXCLUSIVE lock has its event destroyed without
// advancing worker_outbox_trim.
func TestExerciseIsolation_1_DropOutboxPartitionLosesInFlightEvent(t *testing.T) {
	s := isoSetupPersistence(t, "iso_ex_1")
	ctx := t.Context()

	watcherStore, err := NewPersistence(ctx, s.pool)
	if err != nil {
		t.Fatalf("NewPersistence(watcherStore) failed: %v", err)
	}
	t.Cleanup(watcherStore.Close)

	old := time.Now().UTC().Add(-2*outboxRetentionAge - 2*outboxPartitionInterval)
	if err := s.createWorkerOutboxPartitions(ctx, old); err != nil {
		t.Fatalf("createWorkerOutboxPartitions(old) failed: %v", err)
	}
	partName := workerOutboxPartitionName(old)

	watch, err := watcherStore.WatchWorkers(ctx)
	if err != nil {
		t.Fatalf("WatchWorkers failed: %v", err)
	}
	defer watch.Close()

	lostWorker := isoNewWorker("lost-worker")
	payload, err := marshalWorkerEvent(store.WorkerEventCreated, lostWorker)
	if err != nil {
		t.Fatalf("marshalWorkerEvent failed: %v", err)
	}
	wTx, err := s.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin(wTx) failed: %v", err)
	}
	defer wTx.Rollback(ctx) //nolint:errcheck
	var writerXID string
	if err := wTx.QueryRow(ctx,
		`INSERT INTO worker_outbox (created_at, payload) VALUES ($1, $2) RETURNING xid::text`,
		old, payload,
	).Scan(&writerXID); err != nil {
		t.Fatalf("inserting in-flight outbox row failed: %v", err)
	}

	dropDone := make(chan error, 1)
	go func() {
		dropDone <- s.dropWorkerOutboxPartition(ctx, s.ownerPool, partName)
	}()

	isoWaitForLockWaiter(t, s.pool)

	if err := wTx.Commit(ctx); err != nil {
		t.Fatalf("Commit(wTx) failed: %v", err)
	}
	if err := <-dropDone; err != nil {
		t.Fatalf("dropWorkerOutboxPartition failed: %v", err)
	}

	var trimXID string
	if err := s.pool.QueryRow(ctx,
		`SELECT COALESCE((SELECT xid::text FROM worker_outbox_trim), '0')`,
	).Scan(&trimXID); err != nil {
		t.Fatalf("querying worker_outbox_trim: %v", err)
	}

	if _, err := s.CreateWorker(ctx, isoNewWorker("after-drop-worker")); err != nil {
		t.Fatalf("CreateWorker(after-drop-worker) failed: %v", err)
	}

	select {
	case ev, ok := <-watch.Events:
		if !ok {
			t.Fatalf("expected bug reproduction, but watch closed for resync")
		}
		if got := ev.Worker.GetMetadata().GetName(); got != "after-drop-worker" {
			t.Fatalf("watch delivered %q, want after-drop-worker", got)
		}
		t.Logf("REPRODUCED Bug 1: in-flight writer xid=%s dropped with partition %s, worker_outbox_trim=%s; watcher skipped lost-worker and delivered %q",
			writerXID, partName, trimXID, ev.Worker.GetMetadata().GetName())
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for watch event")
	}
}

// 2. UpdateActorAndRelease takes separate READ COMMITTED snapshots in
// releaseActor and updateActor without locking actors first, and also mutates
// workers/worker_assignments before checking the actor precondition.
func TestExerciseIsolation_2_UpdateActorAndReleaseSplitSnapshotAndPrematureWorkerLock(t *testing.T) {
	t.Run("split_snapshot_orphans_assignment", func(t *testing.T) {
		ctx := t.Context()
		tracer := &sqlHookTracer{}
		pool := isoMigratedPool(t, "iso_ex_2a")
		tracedPool := isoOpenPool(t, "iso_ex_2a", tracer, "")

		s, err := NewPersistence(ctx, tracedPool)
		if err != nil {
			t.Fatalf("NewPersistence failed: %v", err)
		}
		t.Cleanup(s.Close)
		setTestPolicyManager(t, s)

		other, err := NewPersistence(ctx, pool)
		if err != nil {
			t.Fatalf("NewPersistence(other) failed: %v", err)
		}
		t.Cleanup(other.Close)
		setTestPolicyManager(t, other)

		createTestAtespace(t, other, "team-a")
		if _, err := other.CreateWorker(ctx, isoNewWorker("w1")); err != nil {
			t.Fatalf("CreateWorker failed: %v", err)
		}
		a1, err := other.CreateActor(ctx, isoNewActor("team-a", "a1"))
		if err != nil {
			t.Fatalf("CreateActor failed: %v", err)
		}
		aRef := resources.ActorRefFromActor(a1)

		hookFired := false
		tracer.onEnd = func(ctx context.Context, sql string) {
			if !hookFired && strings.Contains(sql, "SELECT proto, worker_name FROM worker_assignments WHERE actor_uid = $1") {
				hookFired = true
				if _, err := other.BindActorToWorker(
					ctx,
					"w1",
					isoNewAssignment(a1),
					nil,
					aRef,
					store.PreconditionFrom(a1),
					func(actor *ateapi.Actor) error {
						actor.Status.State = ateapi.ActorState_ACTOR_STATE_RUNNING
						actor.Status.WorkerAssignment = &ateapi.WorkerAssignment{
							Worker: &ateapi.ObjectRef{Name: "w1"},
						}
						return nil
					},
				); err != nil {
					t.Errorf("concurrent BindActorToWorker failed: %v", err)
				}
			}
		}

		updated, err := s.UpdateActorAndRelease(
			ctx,
			aRef,
			store.Precondition{UID: a1.GetMetadata().GetUid(), Version: 2},
			func(actor *ateapi.Actor) error {
				actor.Status.State = ateapi.ActorState_ACTOR_STATE_SUSPENDED
				actor.Status.WorkerAssignment = nil
				return nil
			},
		)
		if err != nil {
			t.Fatalf("UpdateActorAndRelease returned error: %v", err)
		}
		w1, err := other.GetWorker(ctx, "w1")
		if err != nil {
			t.Fatalf("GetWorker failed: %v", err)
		}
		assignments, err := other.ListWorkerAssignments(ctx, "w1", store.ListOptions{})
		if err != nil {
			t.Fatalf("ListWorkerAssignments failed: %v", err)
		}
		if updated.GetStatus().GetWorkerAssignment() != nil || len(assignments.Items) != 1 {
			t.Fatalf("expected orphaned assignment, got actor.WorkerAssignment=%v, assignments=%d",
				updated.GetStatus().GetWorkerAssignment(), len(assignments.Items))
		}
		t.Logf("REPRODUCED Bug 2a: actor %s has WorkerAssignment=nil (version=%d), while w1 still holds %d assignment and Allocated=%+v",
			aRef, updated.GetMetadata().GetVersion(), len(assignments.Items), w1.GetStatus().GetAllocated())
	})

	t.Run("stale_actor_precondition_still_locks_and_writes_worker", func(t *testing.T) {
		ctx := t.Context()
		tracer := &sqlHookTracer{}
		pool := isoMigratedPool(t, "iso_ex_2b")
		tracedPool := isoOpenPool(t, "iso_ex_2b", tracer, "")

		s, err := NewPersistence(ctx, tracedPool)
		if err != nil {
			t.Fatalf("NewPersistence failed: %v", err)
		}
		t.Cleanup(s.Close)
		setTestPolicyManager(t, s)

		other, err := NewPersistence(ctx, pool)
		if err != nil {
			t.Fatalf("NewPersistence(other) failed: %v", err)
		}
		t.Cleanup(other.Close)
		setTestPolicyManager(t, other)

		createTestAtespace(t, other, "team-a")
		if _, err := other.CreateWorker(ctx, isoNewWorker("w1")); err != nil {
			t.Fatalf("CreateWorker(w1) failed: %v", err)
		}
		if _, err := other.CreateWorker(ctx, isoNewWorker("w2")); err != nil {
			t.Fatalf("CreateWorker(w2) failed: %v", err)
		}
		a1, err := other.CreateActor(ctx, isoNewActor("team-a", "a1"))
		if err != nil {
			t.Fatalf("CreateActor failed: %v", err)
		}
		aRef := resources.ActorRefFromActor(a1)

		boundW1, err := other.BindActorToWorker(ctx, "w1", isoNewAssignment(a1), nil, aRef, store.PreconditionFrom(a1), nil)
		if err != nil {
			t.Fatalf("BindActorToWorker(w1) failed: %v", err)
		}
		releasedW1, err := other.UpdateActorAndRelease(ctx, aRef, store.PreconditionFrom(boundW1), nil)
		if err != nil {
			t.Fatalf("UpdateActorAndRelease(w1) failed: %v", err)
		}
		if _, err := other.BindActorToWorker(ctx, "w2", isoNewAssignment(releasedW1), nil, aRef, store.PreconditionFrom(releasedW1), nil); err != nil {
			t.Fatalf("BindActorToWorker(w2) failed: %v", err)
		}

		var workerUpdatedBeforeCheck atomic.Bool
		tracer.onEnd = func(ctx context.Context, sql string) {
			if strings.Contains(sql, "UPDATE workers SET version = $1") {
				workerUpdatedBeforeCheck.Store(true)
			}
		}

		_, err = s.UpdateActorAndRelease(ctx, aRef, store.PreconditionFrom(boundW1), nil)
		if !errors.Is(err, store.ErrVersionConflict) {
			t.Fatalf("UpdateActorAndRelease = %v, want ErrVersionConflict", err)
		}
		if !workerUpdatedBeforeCheck.Load() {
			t.Fatal("expected releaseActor to execute UPDATE workers before checking actor precondition")
		}
		t.Logf("REPRODUCED Bug 2b: stale UpdateActorAndRelease(v2) locked w2, deleted its worker_assignments row, and executed UPDATE workers before checking actor precondition and rolling back")
	})
}

// 3. MarkActorForDeletion, MarkActorTemplateForDeletion, and MarkTagForDeletion
// fail with ErrVersionConflict even when DeletePreconditions{Version: 0} waives
// the version check.
func TestExerciseIsolation_3_MarkForDeletionWaivedVersionFails(t *testing.T) {
	ctx := t.Context()
	tracer := &sqlHookTracer{}
	pool := isoMigratedPool(t, "iso_ex_3")
	tracedPool := isoOpenPool(t, "iso_ex_3", tracer, "")

	s, err := NewPersistence(ctx, tracedPool)
	if err != nil {
		t.Fatalf("NewPersistence failed: %v", err)
	}
	t.Cleanup(s.Close)
	setTestPolicyManager(t, s)

	other, err := NewPersistence(ctx, pool)
	if err != nil {
		t.Fatalf("NewPersistence(other) failed: %v", err)
	}
	t.Cleanup(other.Close)
	setTestPolicyManager(t, other)

	createTestAtespace(t, other, "team-a")
	a1, err := other.CreateActor(ctx, isoNewActor("team-a", "a1"))
	if err != nil {
		t.Fatalf("CreateActor failed: %v", err)
	}
	aRef := resources.ActorRefFromActor(a1)

	hookFired := false
	tracer.onEnd = func(ctx context.Context, sql string) {
		if !hookFired && strings.Contains(sql, "SELECT uid, version, proto FROM actors WHERE atespace = $1 AND name = $2") {
			hookFired = true
			if _, err := other.UpdateActor(ctx, aRef, store.PreconditionFrom(a1), nil); err != nil {
				t.Errorf("concurrent UpdateActor failed: %v", err)
			}
		}
	}

	_, err = s.MarkActorForDeletion(ctx, aRef, store.DeletePreconditions{}, nil)
	if !errors.Is(err, store.ErrVersionConflict) {
		t.Fatalf("expected ErrVersionConflict bug, got: %v", err)
	}
	t.Logf("REPRODUCED Bug 3: MarkActorForDeletion with DeletePreconditions{Version: 0} failed with %v when concurrent UpdateActor committed between SELECT and UPDATE", err)
}

// 4. UpdateActor / UpdateActorTemplate / UpdateTag / UpdateEgressPolicy (and
// BindActorToWorker's updateActor) misclassify concurrent Delete (ErrNotFound)
// and Delete+Create (ErrUIDConflict) between SELECT and UPDATE as ErrVersionConflict.
func TestExerciseIsolation_4_UpdateMisclassifiesConcurrentDeleteAndRecreate(t *testing.T) {
	s := isoSetupPersistence(t, "iso_ex_4")
	ctx := t.Context()
	createTestAtespace(t, s, "team-a")

	a1, err := s.CreateActor(ctx, isoNewActor("team-a", "a1"))
	if err != nil {
		t.Fatalf("CreateActor failed: %v", err)
	}
	aRef := resources.ActorRefFromActor(a1)

	_, errDel := s.UpdateActor(ctx, aRef, store.PreconditionFrom(a1), func(actor *ateapi.Actor) error {
		if _, derr := s.DeleteActor(ctx, aRef, store.DeletePreconditions{}); derr != nil {
			t.Fatalf("concurrent DeleteActor failed: %v", derr)
		}
		return nil
	})
	if !errors.Is(errDel, store.ErrVersionConflict) {
		t.Fatalf("expected ErrVersionConflict bug on deleted actor, got %v", errDel)
	}

	a2, err := s.CreateActor(ctx, isoNewActor("team-a", "a1"))
	if err != nil {
		t.Fatalf("CreateActor(a2) failed: %v", err)
	}
	_, errABA := s.UpdateActor(ctx, aRef, store.PreconditionFrom(a2), func(actor *ateapi.Actor) error {
		if _, derr := s.DeleteActor(ctx, aRef, store.DeletePreconditions{}); derr != nil {
			t.Fatalf("concurrent DeleteActor failed: %v", derr)
		}
		if _, cerr := s.CreateActor(ctx, isoNewActor("team-a", "a1")); cerr != nil {
			t.Fatalf("concurrent CreateActor failed: %v", cerr)
		}
		return nil
	})
	if !errors.Is(errABA, store.ErrVersionConflict) {
		t.Fatalf("expected ErrVersionConflict bug on ABA recreated actor, got %v", errABA)
	}
	t.Logf("REPRODUCED Bug 4: UpdateActor returned %v (instead of ErrNotFound) on concurrent delete, and %v (instead of ErrUIDConflict) on concurrent delete+recreate",
		errDel, errABA)
}

// 5. DeleteTag / DeleteActorTemplate / DeleteEgressPolicy / DeleteAtespace /
// DeleteAtespaceAccessPolicy split DELETE and diagnostic SELECT across two
// statements, returning ErrVersionConflict on an unconditional delete
// (DeletePreconditions{}) when Create commits in between.
func TestExerciseIsolation_5_SplitDeleteErrorMappingAnomaly(t *testing.T) {
	ctx := t.Context()
	tracer := &sqlHookTracer{}
	pool := isoMigratedPool(t, "iso_ex_5")
	tracedPool := isoOpenPool(t, "iso_ex_5", tracer, "")

	s, err := NewPersistence(ctx, tracedPool)
	if err != nil {
		t.Fatalf("NewPersistence failed: %v", err)
	}
	t.Cleanup(s.Close)
	setTestPolicyManager(t, s)

	other, err := NewPersistence(ctx, pool)
	if err != nil {
		t.Fatalf("NewPersistence(other) failed: %v", err)
	}
	t.Cleanup(other.Close)
	setTestPolicyManager(t, other)

	createTestAtespace(t, other, "team-a")
	tagRef := resources.TagRef{Atespace: "team-a", Name: "v1"}

	hookFired := false
	tracer.onEnd = func(ctx context.Context, sql string) {
		if !hookFired && strings.Contains(sql, "DELETE FROM tags") {
			hookFired = true
			if _, err := other.CreateTag(ctx, &ateapi.Tag{
				Metadata: &ateapi.ResourceMetadata{Atespace: "team-a", Name: "v1"},
			}); err != nil {
				t.Errorf("concurrent CreateTag failed: %v", err)
			}
		}
	}

	_, err = s.DeleteTag(ctx, tagRef, store.DeletePreconditions{})
	if !errors.Is(err, store.ErrVersionConflict) {
		t.Fatalf("expected ErrVersionConflict bug, got %v", err)
	}
	t.Logf("REPRODUCED Bug 5: unconditional DeleteTag(DeletePreconditions{}) returned %v and left the tag undeleted", err)
}

// 6. WatchWorkers dual-delivery (in-process publishLocally + xmin-fenced
// worker_outbox poll) causes workercache to resurrect a deleted worker when an
// unrelated transaction holds xmin between CreateWorker and DeleteWorker.
func TestExerciseIsolation_6_WorkercacheResurrectsDeletedWorkerViaOutboxReplay(t *testing.T) {
	s := isoSetupPersistence(t, "iso_ex_6")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	cache := workercache.New(s, time.Hour)
	if err := cache.Start(ctx); err != nil {
		t.Fatalf("cache.Start failed: %v", err)
	}

	if _, err := s.CreateWorker(ctx, isoNewWorker("w-zombie")); err != nil {
		t.Fatalf("CreateWorker failed: %v", err)
	}

	fenceTx, err := s.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin(fenceTx) failed: %v", err)
	}
	defer fenceTx.Rollback(ctx) //nolint:errcheck
	if _, err := fenceTx.Exec(ctx, `SELECT pg_current_xact_id()`); err != nil {
		t.Fatalf("assigning fenceTx xid failed: %v", err)
	}

	if _, err := s.DeleteWorker(ctx, "w-zombie", store.DeletePreconditions{}); err != nil {
		t.Fatalf("DeleteWorker failed: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	resurrected := false
	for time.Now().Before(deadline) {
		if _, err := cache.Worker("w-zombie"); err == nil {
			resurrected = true
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !resurrected {
		t.Fatal("expected workercache to resurrect w-zombie on outbox replay")
	}
	t.Logf("REPRODUCED Bug 6: deleted worker 'w-zombie' was resurrected in workercache by outbox replay while DeleteWorker's xid was fenced behind an unrelated open transaction")
}

// 7. Exercising the store under SERIALIZABLE isolation reproduces immediate
// SQLSTATE 40001 (serialization_failure) errors across worker binds and
// access policy updates.
func TestExerciseIsolation_7_SerializableModeFailures(t *testing.T) {
	ctx := t.Context()

	_ = isoMigratedPool(t, "iso_ex_serializable")
	serialPool := isoOpenPool(t, "iso_ex_serializable", nil, "serializable")

	s, err := NewPersistence(ctx, serialPool)
	if err != nil {
		t.Fatalf("NewPersistence failed: %v", err)
	}
	t.Cleanup(s.Close)
	setTestPolicyManager(t, s)

	createTestAtespace(t, s, "team-a")
	if _, err := s.CreateWorker(ctx, isoNewWorker("w-ser")); err != nil {
		t.Fatalf("CreateWorker failed: %v", err)
	}
	a1, err := s.CreateActor(ctx, isoNewActor("team-a", "a1"))
	if err != nil {
		t.Fatalf("CreateActor(a1) failed: %v", err)
	}
	a2, err := s.CreateActor(ctx, isoNewActor("team-a", "a2"))
	if err != nil {
		t.Fatalf("CreateActor(a2) failed: %v", err)
	}

	admitEntered := make(chan struct{})
	releaseAdmit := make(chan struct{})
	var wg sync.WaitGroup
	var err1, err2 error
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, err1 = s.BindActorToWorker(ctx, "w-ser", isoNewAssignment(a1), func(*ateapi.Worker) error {
			close(admitEntered)
			<-releaseAdmit
			return nil
		}, resources.ActorRefFromActor(a1), store.PreconditionFrom(a1), nil)
	}()
	<-admitEntered
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, err2 = s.BindActorToWorker(ctx, "w-ser", isoNewAssignment(a2), nil, resources.ActorRefFromActor(a2), store.PreconditionFrom(a2), nil)
	}()
	isoWaitForLockWaiter(t, serialPool)
	close(releaseAdmit)
	wg.Wait()

	if err1 != nil {
		t.Fatalf("first BindActorToWorker failed: %v", err1)
	}
	if pgErrCode(err2) != "40001" {
		t.Fatalf("second BindActorToWorker under SERIALIZABLE = %v, want SQLSTATE 40001", err2)
	}
	t.Logf("REPRODUCED Bug 7a (SERIALIZABLE BindActorToWorker): %v", err2)

	createdPol, err := s.CreateGlobalAccessPolicy(ctx, &ateapi.AccessPolicy{
		Metadata: &ateapi.ResourceMetadata{Name: "default"},
		Bindings: []*ateapi.Binding{{Role: authz.RoleOwner, Members: []string{"user:alice"}}},
	})
	if err != nil {
		t.Fatalf("CreateGlobalAccessPolicy failed: %v", err)
	}
	pre := store.PreconditionFrom(createdPol)
	polEntered := make(chan struct{})
	polRelease := make(chan struct{})
	var polErr1, polErr2 error
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, polErr1 = s.UpdateGlobalAccessPolicy(ctx, pre, func(ap *ateapi.AccessPolicy) error {
			close(polEntered)
			<-polRelease
			ap.Bindings = []*ateapi.Binding{{Role: authz.RoleOwner, Members: []string{"user:bob"}}}
			return nil
		})
	}()
	<-polEntered
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, polErr2 = s.UpdateGlobalAccessPolicy(ctx, pre, func(ap *ateapi.AccessPolicy) error {
			ap.Bindings = []*ateapi.Binding{{Role: authz.RoleOwner, Members: []string{"user:carol"}}}
			return nil
		})
	}()
	isoWaitForLockWaiter(t, serialPool)
	close(polRelease)
	wg.Wait()

	if polErr1 != nil {
		t.Fatalf("first UpdateGlobalAccessPolicy failed: %v", polErr1)
	}
	if pgErrCode(polErr2) != "40001" || errors.Is(polErr2, store.ErrVersionConflict) {
		t.Fatalf("second UpdateGlobalAccessPolicy under SERIALIZABLE = %v, want unmapped SQLSTATE 40001 (not ErrVersionConflict)", polErr2)
	}
	t.Logf("REPRODUCED Bug 7b (SERIALIZABLE UpdateGlobalAccessPolicy): %v (not mapped to ErrVersionConflict)", polErr2)
}
