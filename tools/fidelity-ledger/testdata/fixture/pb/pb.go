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

// Package pb is shaped like protoc-gen-go-grpc output for two services in
// proto package fix.
package pb

import (
	"context"

	"google.golang.org/grpc"
)

type PingRequest struct{}
type PingResponse struct{}
type ListRequest struct{}
type ListResponse struct{}
type EchoRequest struct{}
type EchoResponse struct{}
type CountRequest struct{}
type CountResponse struct{}

const (
	Alpha_Ping_FullMethodName  = "/fix.Alpha/Ping"
	Alpha_List_FullMethodName  = "/fix.Alpha/List"
	Alpha_Count_FullMethodName = "/fix.Alpha/Count"
	Beta_Ping_FullMethodName   = "/fix.Beta/Ping"
	Beta_Echo_FullMethodName   = "/fix.Beta/Echo"
)

type AlphaClient interface {
	Ping(ctx context.Context, in *PingRequest, opts ...grpc.CallOption) (*PingResponse, error)
	List(ctx context.Context, in *ListRequest, opts ...grpc.CallOption) (*ListResponse, error)
	Count(ctx context.Context, in *CountRequest, opts ...grpc.CallOption) (*CountResponse, error)
}

// BetaClient's Ping has the name and request of AlphaClient's.
type BetaClient interface {
	Ping(ctx context.Context, in *PingRequest, opts ...grpc.CallOption) (*PingResponse, error)
	Echo(ctx context.Context, in *EchoRequest, opts ...grpc.CallOption) (*EchoResponse, error)
}
