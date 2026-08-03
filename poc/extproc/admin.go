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

package extproc

import (
	"encoding/json"
	"net/http"
	"sort"
)

// AdminHandler serves the PoC's out-of-band endpoints:
//
//	/stats    counters, so the harness asserts on measurements rather than logs
//	/healthz  readiness, gated on a policy snapshot being loaded
//	/echo     reflects the request back as JSON
//
// /echo is here so the harness can see exactly which headers reached the
// destination — which is the only way to verify that credential injection
// happened and that the X-Ate-* headers were stripped. In a real deployment
// this is an unrelated upstream, not part of this process.
func AdminHandler(store *Store, stats *Stats) http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("/stats", func(w http.ResponseWriter, r *http.Request) {
		snap := store.Load()
		rev, size := 0, 0
		if snap != nil {
			rev, size = snap.Rev, snap.Len()
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"policyRev":   rev,
			"policyCount": size,
			"counters":    stats.Snapshot(),
		})
	})

	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		// Fail closed until the first snapshot lands. Envoy must not route to a
		// replica that would deny everything.
		if !store.Ready() {
			http.Error(w, "no policy snapshot", http.StatusServiceUnavailable)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ready": true})
	})

	mux.HandleFunc("/echo", func(w http.ResponseWriter, r *http.Request) {
		hdrs := make(map[string]string, len(r.Header))
		names := make([]string, 0, len(r.Header))
		for k, v := range r.Header {
			if len(v) > 0 {
				hdrs[k] = v[0]
			}
			names = append(names, k)
		}
		sort.Strings(names)
		writeJSON(w, http.StatusOK, map[string]any{
			"method":      r.Method,
			"host":        r.Host,
			"path":        r.URL.Path,
			"headers":     hdrs,
			"headerNames": names,
		})
	})

	return mux
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}
