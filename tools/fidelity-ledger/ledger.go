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
	"bytes"
	"errors"
	"fmt"
	"maps"
	"os"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"
)

// ledger is the checked-in record of how the benchmark fake data plane
// stands in for each call the real one makes across the control-plane
// boundary.
type ledger struct {
	Scans []scan  `yaml:"scans"`
	Calls []entry `yaml:"calls"`
	Gaps  []gap   `yaml:"gaps"`
}

// The statuses an entry can have.
const (
	emulated      = "emulated"
	notEmulated   = "not_emulated"
	notApplicable = "not_applicable"
)

// entry classifies one gRPC method.
type entry struct {
	RPC     string   `yaml:"rpc"`
	Callers []string `yaml:"callers"`
	Status  string   `yaml:"status"`
	// Emulation says how the fakes make the call; required when emulated.
	Emulation string `yaml:"emulation,omitempty"`
	// Reason says why the benchmark needs no stand-in; required when
	// not_applicable.
	Reason string `yaml:"reason,omitempty"`
	// Gaps name the gaps that make the fakes' cost differ from the real
	// call's; at least one is required when not_emulated.
	Gaps []string `yaml:"gaps,omitempty"`
}

// gap is a known way the fakes cost ate-api-server something other than
// the real data plane does.
type gap struct {
	ID     string `yaml:"id"`
	Brief  string `yaml:"brief"`
	Impact string `yaml:"impact"`
	Plan   string `yaml:"plan"`
}

func readLedger(file string) (*ledger, error) {
	data, err := os.ReadFile(file)
	if err != nil {
		return nil, err
	}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	var l ledger
	if err := dec.Decode(&l); err != nil {
		return nil, fmt.Errorf("while parsing %s: %w", file, err)
	}
	return &l, nil
}

// validate reports every entry or gap that is incomplete or inconsistent,
// whatever the code calls.
func (l *ledger) validate() error {
	var errs []error
	if len(l.Scans) == 0 {
		errs = append(errs, errors.New("no scans"))
	}
	for _, s := range l.Scans {
		if s.Name == "" || len(s.Roots) == 0 || len(s.Services) == 0 {
			errs = append(errs, fmt.Errorf("scan %q needs a name, roots and services", s.Name))
		}
	}
	gaps := map[string]bool{}
	for _, g := range l.Gaps {
		switch {
		case g.ID == "":
			errs = append(errs, errors.New("a gap has no id"))
		case gaps[g.ID]:
			errs = append(errs, fmt.Errorf("gap %s is listed twice", g.ID))
		case g.Brief == "" || g.Impact == "" || g.Plan == "":
			errs = append(errs, fmt.Errorf("gap %s needs a brief, an impact and a plan", g.ID))
		}
		gaps[g.ID] = true
	}
	rpcs := map[string]bool{}
	for _, e := range l.Calls {
		if rpcs[e.RPC] {
			errs = append(errs, fmt.Errorf("%s is listed twice", e.RPC))
		}
		rpcs[e.RPC] = true
		switch e.Status {
		case emulated:
			if e.Emulation == "" {
				errs = append(errs, fmt.Errorf("%s is emulated but does not say how", e.RPC))
			}
		case notEmulated:
			if len(e.Gaps) == 0 {
				errs = append(errs, fmt.Errorf("%s is not emulated but names no gap", e.RPC))
			}
		case notApplicable:
			if e.Reason == "" {
				errs = append(errs, fmt.Errorf("%s is not applicable but gives no reason", e.RPC))
			}
			if len(e.Gaps) > 0 {
				errs = append(errs, fmt.Errorf("%s is not applicable but names gaps", e.RPC))
			}
		default:
			errs = append(errs, fmt.Errorf("%s has status %q, want %s, %s or %s", e.RPC, e.Status, emulated, notEmulated, notApplicable))
		}
		for _, id := range e.Gaps {
			if !gaps[id] {
				errs = append(errs, fmt.Errorf("%s names gap %s, which is not listed", e.RPC, id))
			}
		}
	}
	return errors.Join(errs...)
}

// diff reports how the ledger's calls differ from found: calls it lacks,
// calls it lists that no longer happen, and calls whose callers changed. For
// the calls it lacks, stubs holds entries for their author to classify.
func (l *ledger) diff(found calls) (problems []string, stubs string) {
	listed := calls{}
	for _, e := range l.Calls {
		listed[e.RPC] = slices.Sorted(slices.Values(e.Callers))
	}
	var b strings.Builder
	for _, rpc := range slices.Sorted(maps.Keys(found)) {
		got, ok := listed[rpc]
		switch {
		case !ok:
			problems = append(problems, fmt.Sprintf("%s is called from %s, but the ledger does not list it", rpc, strings.Join(found[rpc], ", ")))
			fmt.Fprintf(&b, "  - rpc: %s\n    callers:\n", rpc)
			for _, c := range found[rpc] {
				fmt.Fprintf(&b, "      - %s\n", c)
			}
			fmt.Fprintf(&b, "    status: %s | %s | %s\n", emulated, notEmulated, notApplicable)
		case !slices.Equal(got, found[rpc]):
			problems = append(problems, fmt.Sprintf("%s is called from %s, but the ledger lists %s; update its callers, and its status if the new callers change what the fakes must do", rpc, strings.Join(found[rpc], ", "), strings.Join(got, ", ")))
		}
	}
	for _, rpc := range slices.Sorted(maps.Keys(listed)) {
		if _, ok := found[rpc]; !ok {
			problems = append(problems, fmt.Sprintf("%s is listed, but nothing in the scans calls it any more; remove it, and any gap only it names", rpc))
		}
	}
	return problems, b.String()
}
