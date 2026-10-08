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
	"fmt"
	"go/ast"
	"go/constant"
	"go/types"
	"os"
	"path"
	"slices"
	"strings"

	"golang.org/x/tools/go/packages"
)

// scan is one side of the control-plane boundary: the packages that make
// calls across it, and the gRPC services whose calls count.
type scan struct {
	Name string `yaml:"name"`
	// Roots are package patterns, relative to the repository root. Every
	// package of this module that a root imports, directly or not, is
	// scanned with it, so a call made in a shared helper still counts.
	Roots []string `yaml:"roots"`
	// Services are full proto service names, such as atelet.AteomHerder, or
	// a proto package followed by ".*" for all of its services.
	Services []string `yaml:"services"`
}

// matches reports whether the scan counts calls to service.
func (s scan) matches(service string) bool {
	for _, want := range s.Services {
		if pkg, ok := strings.CutSuffix(want, ".*"); ok {
			if strings.HasPrefix(service, pkg+".") && !strings.Contains(service[len(pkg)+1:], ".") {
				return true
			}
		} else if service == want {
			return true
		}
	}
	return false
}

// calls maps a gRPC method, such as ateapi.WorkerService/RegisterWorker, to
// the packages that call it, relative to the module root and sorted.
type calls map[string][]string

// findCalls loads the scans' roots from the module at root and returns every
// call they make to the scans' services. A call is a method call whose
// signature is a generated gRPC client method's, (ctx, *Request,
// ...grpc.CallOption), whether made through the generated client or through
// a narrower interface of the caller's own.
func findCalls(root string, scans []scan) (calls, error) {
	out := calls{}
	for _, s := range scans {
		cfg := &packages.Config{
			Mode: packages.NeedName | packages.NeedModule | packages.NeedImports | packages.NeedDeps |
				packages.NeedTypes | packages.NeedSyntax | packages.NeedTypesInfo,
			Dir: root,
			// The data plane runs on Linux; ateom-gvisor builds only there.
			Env: append(os.Environ(), "GOOS=linux"),
		}
		pkgs, err := packages.Load(cfg, s.Roots...)
		if err != nil {
			return nil, fmt.Errorf("while loading scan %q: %w", s.Name, err)
		}
		var errs []string
		methods := methodIndex{}
		seen := map[string]bool{}
		packages.Visit(pkgs, nil, func(p *packages.Package) {
			if seen[p.ID] {
				return
			}
			seen[p.ID] = true
			for _, e := range p.Errors {
				errs = append(errs, e.Error())
			}
			if p.Module == nil || !p.Module.Main {
				return
			}
			caller := strings.TrimPrefix(strings.TrimPrefix(p.PkgPath, p.Module.Path), "/")
			if caller == "" {
				caller = "."
			}
			for _, f := range p.Syntax {
				ast.Inspect(f, func(n ast.Node) bool {
					var matched []string
					for _, m := range methods.lookup(p.TypesInfo, n) {
						if s.matches(path.Dir(m)) {
							matched = append(matched, m)
						}
					}
					switch {
					case len(matched) == 0:
						return true
					case len(matched) > 1:
						errs = append(errs, fmt.Sprintf("%s: the call could be any of %s; make it through the generated client",
							p.Fset.Position(n.Pos()), strings.Join(matched, ", ")))
						return true
					}
					method := matched[0]
					if !slices.Contains(out[method], caller) {
						out[method] = append(out[method], caller)
					}
					return true
				})
			}
		})
		if len(errs) > 0 {
			return nil, fmt.Errorf("scan %q failed:\n  %s", s.Name, strings.Join(errs, "\n  "))
		}
	}
	for _, callers := range out {
		slices.Sort(callers)
	}
	return out, nil
}

// methodIndex caches, per generated proto package, the gRPC methods each
// client method can stand for.
type methodIndex map[*types.Package]map[clientMethod][]string

// clientMethod is a client method by name and request type: a narrower
// interface of the caller's own shares both with the generated one.
type clientMethod struct {
	name    string
	request types.Type
}

// lookup returns the gRPC methods, such as ateapi.WorkerService/RegisterWorker,
// that n may call: none if n is not a gRPC client call, and more than one if
// n calls through an interface other than the generated client and two
// services have a method of the same name and request.
func (idx methodIndex) lookup(info *types.Info, n ast.Node) []string {
	call, ok := n.(*ast.CallExpr)
	if !ok {
		return nil
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return nil
	}
	selection := info.Selections[sel]
	if selection == nil || selection.Kind() != types.MethodVal {
		return nil
	}
	request, ok := clientRequest(selection.Obj().Type().(*types.Signature))
	if !ok {
		return nil
	}
	pkg := request.Obj().Pkg()
	if idx[pkg] == nil {
		idx[pkg] = indexPackage(pkg)
	}
	methods := idx[pkg][clientMethod{selection.Obj().Name(), request}]
	if recv, ok := types.Unalias(selection.Recv()).(*types.Named); ok && recv.Obj().Pkg() == pkg {
		if service, ok := strings.CutSuffix(recv.Obj().Name(), "Client"); ok {
			for _, m := range methods {
				if dir := path.Dir(m); dir == service || strings.HasSuffix(dir, "."+service) {
					return []string{m}
				}
			}
		}
	}
	return methods
}

// clientRequest returns the request type of a signature shaped like a
// generated unary or server-streaming gRPC client method.
func clientRequest(sig *types.Signature) (*types.Named, bool) {
	params := sig.Params()
	if !sig.Variadic() || params.Len() != 3 {
		return nil, false
	}
	opt, ok := params.At(2).Type().(*types.Slice).Elem().(*types.Named)
	if !ok || opt.Obj().Pkg() == nil || opt.Obj().Pkg().Path() != "google.golang.org/grpc" || opt.Obj().Name() != "CallOption" {
		return nil, false
	}
	ptr, ok := params.At(1).Type().(*types.Pointer)
	if !ok {
		return nil, false
	}
	request, ok := ptr.Elem().(*types.Named)
	return request, ok && request.Obj().Pkg() != nil
}

// indexPackage maps each method of each generated client interface in pkg,
// <Service>Client, to its gRPC methods, from the generated
// <Service>_<Method>_FullMethodName constant.
func indexPackage(pkg *types.Package) map[clientMethod][]string {
	out := map[clientMethod][]string{}
	scope := pkg.Scope()
	for _, name := range scope.Names() {
		service, ok := strings.CutSuffix(name, "Client")
		if !ok {
			continue
		}
		iface, ok := scope.Lookup(name).Type().Underlying().(*types.Interface)
		if !ok {
			continue
		}
		for m := range iface.Methods() {
			request, ok := clientRequest(m.Type().(*types.Signature))
			if !ok {
				continue
			}
			c, ok := scope.Lookup(service + "_" + m.Name() + "_FullMethodName").(*types.Const)
			if !ok || c.Val().Kind() != constant.String {
				continue
			}
			key := clientMethod{m.Name(), request}
			out[key] = append(out[key], strings.TrimPrefix(constant.StringVal(c.Val()), "/"))
		}
	}
	return out
}
