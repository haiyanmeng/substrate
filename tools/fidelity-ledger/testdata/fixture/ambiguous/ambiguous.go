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

// Package ambiguous calls Ping through an interface of its own, which could
// be either service's.
package ambiguous

import (
	"context"

	"example.com/fixture/pb"
	"google.golang.org/grpc"
)

type pinger interface {
	Ping(ctx context.Context, in *pb.PingRequest, opts ...grpc.CallOption) (*pb.PingResponse, error)
}

func Run(ctx context.Context, p pinger) {
	p.Ping(ctx, &pb.PingRequest{})
}
