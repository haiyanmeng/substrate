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
	"testing"

	"go.ate.dev/substrate/api/ateapi/v1"
	"go.ate.dev/substrate/cmd/ateapi/internal/store"
)

// newTestTag returns tagName over its own copy of an actor's external
// snapshot, already finalized.
func newTestTag(tagAtespace, tagName string) *ateapi.Tag {
	return &ateapi.Tag{
		Metadata: &ateapi.ResourceMetadata{Atespace: tagAtespace, Name: tagName},
		Scope:    ateapi.TagScope_TAG_SCOPE_ATESPACE,
		Status: &ateapi.TagStatus{
			Snapshot: &ateapi.Snapshot{
				Generation: 1,
				Owner:      ateapi.SnapshotOwner_SNAPSHOT_OWNER_TAG,
				Fidelity:   ateapi.SnapshotFidelity_SNAPSHOT_FIDELITY_MEMORY,
				DurableSnapshot: &ateapi.SnapshotStorage{
					State:  ateapi.SnapshotStorageState_SNAPSHOT_STORAGE_STATE_COMPLETED,
					Object: &ateapi.ObjectSnapshot{SnapshotUri: "gs://bucket/atespaces/" + tagAtespace + "/tags/" + tagName},
				},
			},
		},
	}
}

func createTestTag(t *testing.T, s *Persistence, tagAtespace, tagName string) *ateapi.Tag {
	t.Helper()
	tag, err := s.CreateTag(context.Background(), newTestTag(tagAtespace, tagName))
	if err != nil {
		t.Fatalf("CreateTag(%s/%s) failed: %v", tagAtespace, tagName, err)
	}
	return tag
}

func TestCreateTag_TagForeignKeyErrors(t *testing.T) {
	s := setupPostgresPersistence(t)
	ctx := context.Background()
	createTestAtespace(t, s, "team-a")

	// A tag in an atespace that does not exist trips the tag's atespace FK.
	_, err := s.CreateTag(ctx, &ateapi.Tag{
		Metadata: &ateapi.ResourceMetadata{Atespace: "gone", Name: "latest"},
		Status:   &ateapi.TagStatus{},
	})
	if !errors.Is(err, store.ErrFailedPrecondition) {
		t.Errorf("missing tag atespace error = %v, want ErrFailedPrecondition", err)
	}
}
