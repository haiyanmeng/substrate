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
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
)

func validLedger() *ledger {
	return &ledger{
		Scans: []scan{{Name: "s", Roots: []string{"./a"}, Services: []string{"fix.*"}}},
		Calls: []entry{
			{RPC: "fix.A/One", Callers: []string{"a"}, Status: emulated, Emulation: "the fake calls it", Gaps: []string{"slow"}},
			{RPC: "fix.A/Two", Callers: []string{"a"}, Status: notEmulated, Gaps: []string{"slow"}},
			{RPC: "fix.A/Three", Callers: []string{"a"}, Status: notApplicable, Reason: "not in the benchmark"},
		},
		Gaps: []gap{{ID: "slow", Brief: "b", Impact: "i", Plan: "p"}},
	}
}

func TestValidate(t *testing.T) {
	if err := validLedger().validate(); err != nil {
		t.Fatalf("validate() = %v, want nil", err)
	}
	for _, tc := range []struct {
		name   string
		mutate func(*ledger)
		want   string
	}{
		{"no scans", func(l *ledger) { l.Scans = nil }, "no scans"},
		{"scan without services", func(l *ledger) { l.Scans[0].Services = nil }, `scan "s" needs`},
		{"emulated without how", func(l *ledger) { l.Calls[0].Emulation = "" }, "fix.A/One is emulated but does not say how"},
		{"not emulated without gap", func(l *ledger) { l.Calls[1].Gaps = nil }, "fix.A/Two is not emulated but names no gap"},
		{"not applicable without reason", func(l *ledger) { l.Calls[2].Reason = "" }, "fix.A/Three is not applicable but gives no reason"},
		{"not applicable with gap", func(l *ledger) { l.Calls[2].Gaps = []string{"slow"} }, "fix.A/Three is not applicable but names gaps"},
		{"unknown status", func(l *ledger) { l.Calls[0].Status = "partial" }, `status "partial"`},
		{"unknown gap", func(l *ledger) { l.Calls[0].Gaps = []string{"fast"} }, "names gap fast, which is not listed"},
		{"duplicate call", func(l *ledger) { l.Calls = append(l.Calls, l.Calls[0]) }, "fix.A/One is listed twice"},
		{"duplicate gap", func(l *ledger) { l.Gaps = append(l.Gaps, l.Gaps[0]) }, "gap slow is listed twice"},
		{"gap without plan", func(l *ledger) { l.Gaps[0].Plan = "" }, "gap slow needs a brief, an impact and a plan"},
		{"gap without id", func(l *ledger) { l.Gaps[0].ID = "" }, "a gap has no id"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l := validLedger()
			tc.mutate(l)
			if err := l.validate(); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("validate() = %v, want it to contain %q", err, tc.want)
			}
		})
	}
}

func TestDiff(t *testing.T) {
	l := validLedger()
	problems, stubs := l.diff(calls{
		"fix.A/One":  {"a"},
		"fix.A/Two":  {"a", "b"},
		"fix.A/Four": {"b"},
	})
	want := []string{
		"fix.A/Four is called from b, but the ledger does not list it",
		"fix.A/Two is called from a, b, but the ledger lists a; update its callers, and its status if the new callers change what the fakes must do",
		"fix.A/Three is listed, but nothing in the scans calls it any more; remove it, and any gap only it names",
	}
	if diff := cmp.Diff(want, problems); diff != "" {
		t.Errorf("diff problems mismatch (-want +got):\n%s", diff)
	}
	wantStubs := "  - rpc: fix.A/Four\n    callers:\n      - b\n    status: emulated | not_emulated | not_applicable\n"
	if stubs != wantStubs {
		t.Errorf("diff stubs = %q, want %q", stubs, wantStubs)
	}
}

func TestDiffIgnoresCallerOrder(t *testing.T) {
	l := validLedger()
	l.Calls[0].Callers = []string{"b", "a"}
	problems, _ := l.diff(calls{"fix.A/One": {"a", "b"}, "fix.A/Two": {"a"}, "fix.A/Three": {"a"}})
	if len(problems) > 0 {
		t.Errorf("diff = %q, want no problems", problems)
	}
}

func TestReadLedgerRejectsUnknownFields(t *testing.T) {
	file := filepath.Join(t.TempDir(), "ledger.yaml")
	if err := os.WriteFile(file, []byte("calls:\n  - rpc: fix.A/One\n    staus: emulated\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readLedger(file); err == nil || !strings.Contains(err.Error(), "staus") {
		t.Errorf("readLedger error = %v, want it to name the unknown field staus", err)
	}
}

// TestRun checks the fixture's ledger end to end: it matches, and it stops
// matching when the code gains a call it does not list.
func TestRun(t *testing.T) {
	if err := run("testdata/fixture", "ledger.yaml"); err != nil {
		t.Fatalf("run() = %v, want nil", err)
	}
	dir := t.TempDir()
	data, err := os.ReadFile("testdata/fixture/ledger.yaml")
	if err != nil {
		t.Fatal(err)
	}
	trimmed := strings.Replace(string(data), "  - rpc: fix.Alpha/List\n    callers:\n      - helper\n    status: emulated\n    emulation: the fake lists\n", "", 1)
	if trimmed == string(data) {
		t.Fatal("testdata/fixture/ledger.yaml no longer lists fix.Alpha/List as this test expects")
	}
	file := filepath.Join(dir, "ledger.yaml")
	if err := os.WriteFile(file, []byte(trimmed), 0o600); err != nil {
		t.Fatal(err)
	}
	err = run("testdata/fixture", file)
	if err == nil || !strings.Contains(err.Error(), "fix.Alpha/List is called from helper, but the ledger does not list it") {
		t.Errorf("run() = %v, want it to report the unlisted fix.Alpha/List", err)
	}
}
