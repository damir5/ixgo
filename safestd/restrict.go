/*
 * Copyright (c) 2026 The GoPlus Authors (goplus.org). All rights reserved.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package safestd

import (
	"errors"
	"fmt"
	"go/types"
	"io"
	"io/fs"
	"slices"

	"github.com/goplus/ixgo"
)

// ErrImport is returned, wrapped in the type check error, for an import the
// program may not use.
var ErrImport = errors.New("safestd: import not allowed")

// Restrict lets programs run in ctx import only Packages and the packages in
// allow, such as the host's tool packages, plus DeterministicPackages when
// Deterministic set up ctx. It refuses "unsafe", which
// //go:linkname also needs. It stops ixgo from loading any package from
// source files, so a package that is not registered is not found.
func Restrict(ctx *ixgo.Context, allow ...string) {
	ctx.Lookup = func(root, path string) (string, bool) { return "", false }
	b := &ctx.BuildContext
	b.GOROOT, b.GOPATH = "", ""
	b.IsDir = func(string) bool { return false }
	b.HasSubdir = func(root, dir string) (string, bool) { return "", false }
	b.ReadDir = func(dir string) ([]fs.FileInfo, error) { return nil, fs.ErrNotExist }
	b.OpenFile = func(path string) (io.ReadCloser, error) { return nil, fs.ErrNotExist }
	allow = append(slices.Clone(Packages), allow...)
	if ctx.Deterministic {
		allow = append(allow, DeterministicPackages...)
	}
	ctx.Importer = &restricted{next: ctx.Importer, allow: allow}
}

// restricted checks only the program's own imports. The type checker
// imports a package's dependencies while that package's Import call runs,
// so a nested call is a dependency of an allowed package: slices imports
// unsafe, for example.
type restricted struct {
	next  types.Importer
	allow []string
	depth int // ponytail: a Context type checks one program at a time
}

func (r *restricted) Import(path string) (*types.Package, error) {
	if r.depth == 0 && !slices.Contains(r.allow, path) {
		return nil, fmt.Errorf("%w: %q", ErrImport, path)
	}
	r.depth++
	defer func() { r.depth-- }()
	return r.next.Import(path)
}
