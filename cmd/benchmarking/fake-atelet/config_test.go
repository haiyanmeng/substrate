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

package main

import (
	"testing"
	"time"

	"github.com/agent-substrate/substrate/pkg/proto/ateapipb"
	"google.golang.org/protobuf/proto"
)

func TestParseCapacity(t *testing.T) {
	got, err := parseCapacity(5, "cpu=32, memory=128Gi")
	if err != nil {
		t.Fatalf("parseCapacity: %v", err)
	}
	want := &ateapipb.WorkerResources{
		Actors: 5,
		Resources: &ateapipb.Resources{Limits: []*ateapipb.Limits{
			{Name: "cpu", Quantity: "32"},
			{Name: "memory", Quantity: "128Gi"},
		}},
	}
	if !proto.Equal(got, want) {
		t.Errorf("parseCapacity = %v, want %v", got, want)
	}

	actorsOnly, err := parseCapacity(1, "")
	if err != nil || actorsOnly.GetResources() != nil || actorsOnly.GetActors() != 1 {
		t.Errorf("parseCapacity(1, \"\") = %v, %v; want 1 actor and no resources", actorsOnly, err)
	}
}

func TestParseCapacityRejects(t *testing.T) {
	for _, tc := range []struct {
		actors int
		list   string
	}{
		{0, ""},
		{1, "gpu=1"},
		{1, "cpu"},
		{1, "cpu=1,cpu=2"},
		{1, "memory=lots"},
		{1, "cpu=0"},
		{1, "cpu=1000"},
	} {
		if _, err := parseCapacity(tc.actors, tc.list); err == nil {
			t.Errorf("parseCapacity(%d, %q) = nil error, want one", tc.actors, tc.list)
		}
	}
}

func TestResolveDelays(t *testing.T) {
	got, err := resolveDelays(time.Second, -1, 2*time.Second, 0, -1, -1)
	if err != nil {
		t.Fatalf("resolveDelays: %v", err)
	}
	want := delays{run: time.Second, restore: 2 * time.Second, checkpoint: 0, uploadPausedCheckpoint: time.Second, terminate: time.Second}
	if got != want {
		t.Errorf("resolveDelays = %+v, want %+v", got, want)
	}
	if _, err := resolveDelays(-time.Second, -1, -1, -1, -1, -1); err == nil {
		t.Error("resolveDelays accepted a negative default")
	}
}
