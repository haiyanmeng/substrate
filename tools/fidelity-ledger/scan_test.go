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
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
)

func TestFindCalls(t *testing.T) {
	for _, tc := range []struct {
		name string
		scan scan
		want calls
	}{
		{
			name: "every service in the package",
			scan: scan{Name: "all", Roots: []string{"./caller"}, Services: []string{"fix.*"}},
			want: calls{
				// Through each generated client, though both have Ping.
				"fix.Alpha/Ping": {"caller"},
				"fix.Beta/Ping":  {"caller"},
				// Through caller's own narrower interface.
				"fix.Beta/Echo": {"caller"},
				// In a package caller imports.
				"fix.Alpha/List": {"helper"},
			},
		},
		{
			name: "one service",
			scan: scan{Name: "alpha", Roots: []string{"./caller"}, Services: []string{"fix.Alpha"}},
			want: calls{"fix.Alpha/Ping": {"caller"}, "fix.Alpha/List": {"helper"}},
		},
		{
			name: "callers merge across roots",
			scan: scan{Name: "two roots", Roots: []string{"./caller", "./other"}, Services: []string{"fix.Beta"}},
			want: calls{"fix.Beta/Ping": {"caller", "other"}, "fix.Beta/Echo": {"caller"}},
		},
		{
			name: "an ambiguous call counts for the one service in scope",
			scan: scan{Name: "alpha only", Roots: []string{"./ambiguous"}, Services: []string{"fix.Alpha"}},
			want: calls{"fix.Alpha/Ping": {"ambiguous"}},
		},
		{
			name: "a package wildcard is not a prefix",
			scan: scan{Name: "none", Roots: []string{"./caller"}, Services: []string{"fi.*"}},
			want: calls{},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := findCalls("testdata/fixture", []scan{tc.scan})
			if err != nil {
				t.Fatal(err)
			}
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Errorf("findCalls mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestFindCallsErrors(t *testing.T) {
	for _, tc := range []struct {
		name, root, want string
	}{
		{"missing root", "./nosuchpackage", "nosuchpackage"},
		{"ambiguous call", "./ambiguous", "could be any of fix.Alpha/Ping, fix.Beta/Ping"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := findCalls("testdata/fixture", []scan{{Name: tc.name, Roots: []string{tc.root}, Services: []string{"fix.*"}}})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("findCalls error = %v, want it to contain %q", err, tc.want)
			}
		})
	}
}

func TestScanMatches(t *testing.T) {
	s := scan{Services: []string{"ateapi.*", "atelet.AteomHerder"}}
	for service, want := range map[string]bool{
		"ateapi.Control":       true,
		"ateapi.WorkerService": true,
		"atelet.AteomHerder":   true,
		"atelet.AteomSupport":  false,
		"ateapix.Control":      false,
		"ateapi.sub.Control":   false,
		"atelet.AteomHerderX":  false,
	} {
		if got := s.matches(service); got != want {
			t.Errorf("matches(%q) = %v, want %v", service, got, want)
		}
	}
}
