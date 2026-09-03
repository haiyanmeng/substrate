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

package e2e

import (
	"context"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/agent-substrate/substrate/pkg/proto/ateapipb"
)

// egressPolicyName is the only name an EgressPolicy may have: an actor has at
// most one, nested under it.
const egressPolicyName = "default"

// AllowAllEgress grants an actor unrestricted egress.
//
// The MITM egress gateway enforces an EgressPolicy on every request out of an
// actor's tunnel and denies when there is none, so a suite that wants to reach
// the internet through it has to say so. The passthrough gateway has no
// decrypted leg and no policy check, which is why only the MITM lanes call
// this — under passthrough it is harmless but unnecessary.
//
// The policy outlives nothing: it is nested under the actor and deleted with
// it, so there is no cleanup to register.
func AllowAllEgress(t *testing.T, ctx context.Context, clients *Clients, atespace, actor string) {
	t.Helper()
	policy := &ateapipb.EgressPolicy{
		Metadata: &ateapipb.ResourceMetadata{Atespace: atespace, Name: egressPolicyName},
		Rules:    []*ateapipb.EgressRule{{All: &emptypb.Empty{}}},
	}
	req := &ateapipb.CreateActorEgressPolicyRequest{
		Actor:        &ateapipb.ObjectRef{Atespace: atespace, Name: actor},
		EgressPolicy: policy,
	}
	if _, err := clients.SubstrateAPI.CreateActorEgressPolicy(ctx, req); err != nil {
		if status.Code(err) != codes.AlreadyExists {
			t.Fatalf("CreateActorEgressPolicy for actor %s/%s: %v", atespace, actor, err)
		}
		// A rerun against a surviving actor: make the existing policy say the
		// same thing rather than assume it does.
		if _, err := clients.SubstrateAPI.UpdateActorEgressPolicy(ctx, &ateapipb.UpdateActorEgressPolicyRequest{
			Actor:        req.Actor,
			EgressPolicy: policy,
		}); err != nil {
			t.Fatalf("UpdateActorEgressPolicy for actor %s/%s: %v", atespace, actor, err)
		}
	}
}
