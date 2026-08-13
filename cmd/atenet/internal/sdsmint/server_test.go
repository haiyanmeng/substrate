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

package sdsmint

import (
	"testing"
	"time"
)

// respondWait bounds a wait for something the server should do promptly. Every
// caller runs inside a synctest bubble, so this is fake time: only a run that
// is already failing ever spends it.
const respondWait = time.Minute

func testServer(t *testing.T, opts serverOptions) *server {
	t.Helper()
	if opts.Logger == nil {
		opts.Logger = quietLogger()
	}
	if opts.TTL <= 0 {
		opts.TTL = time.Minute
	}
	m := testMinter(t, minterOptions{TTL: opts.TTL})
	return newServer(m, opts)
}
