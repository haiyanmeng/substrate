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

// Command fidelity-ledger checks the benchmark fake data plane's fidelity
// ledger against the code.
//
//	fidelity-ledger -root <repo> <ledger.yaml>
//
// The ledger lists every call the real data plane makes across the
// control-plane boundary, each with whether the fakes in
// cmd/benchmarking/isolate emulate it. The command finds those calls in the
// code with go/packages and fails when the ledger lists a different set, or
// when an entry or gap is incomplete, so a new call is classified in the
// change that adds it.
package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func main() {
	root := flag.String("root", ".", "the repository root; the ledger's roots are relative to it")
	flag.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: fidelity-ledger [-root DIR] <ledger.yaml>")
		flag.PrintDefaults()
	}
	flag.Parse()
	if flag.NArg() != 1 {
		flag.Usage()
		os.Exit(2)
	}
	if err := run(*root, flag.Arg(0)); err != nil {
		fmt.Fprintf(os.Stderr, "FAIL: %v\n", err)
		os.Exit(1)
	}
}

func run(root, file string) error {
	if !filepath.IsAbs(file) {
		file = filepath.Join(root, file)
	}
	l, err := readLedger(file)
	if err != nil {
		return err
	}
	if err := l.validate(); err != nil {
		return fmt.Errorf("%s is incomplete:\n%w", file, err)
	}
	found, err := findCalls(root, l.Scans)
	if err != nil {
		return err
	}
	problems, stubs := l.diff(found)
	if len(problems) > 0 {
		var b strings.Builder
		fmt.Fprintf(&b, "%s does not match the calls the data plane makes:\n", file)
		for _, p := range problems {
			fmt.Fprintf(&b, "  - %s\n", p)
		}
		if stubs != "" {
			fmt.Fprintf(&b, "\nAdd these entries under calls, and classify each:\n\n%s", stubs)
		}
		return errors.New(b.String())
	}
	fmt.Printf("%s matches the %d calls the data plane makes.\n", file, len(found))
	return nil
}
