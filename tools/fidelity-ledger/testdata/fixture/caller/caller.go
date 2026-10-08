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

// Package caller calls Ping through both generated clients, fix.Beta/Echo
// through a narrower interface of its own, and fix.Alpha/List through helper.
package caller

import (
	"context"

	"example.com/fixture/helper"
	"example.com/fixture/pb"
	"google.golang.org/grpc"
)

type echoer interface {
	Echo(ctx context.Context, in *pb.EchoRequest, opts ...grpc.CallOption) (*pb.EchoResponse, error)
}

// notClient has a client method's name and request but no ...grpc.CallOption.
type notClient interface {
	Echo(ctx context.Context, in *pb.EchoRequest) (*pb.EchoResponse, error)
}

func Run(ctx context.Context, a pb.AlphaClient, b pb.BetaClient, e echoer, n notClient) {
	a.Ping(ctx, &pb.PingRequest{})
	b.Ping(ctx, &pb.PingRequest{})
	e.Echo(ctx, &pb.EchoRequest{})
	n.Echo(ctx, &pb.EchoRequest{})
	helper.List(ctx, a)
}
