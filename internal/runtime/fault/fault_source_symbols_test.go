// Copyright 2026 The agentctl Authors.
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

package fault_test

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	gocmp "github.com/google/go-cmp/cmp"
	"golang.org/x/mod/modfile"
	"golang.org/x/tools/go/packages"
)

const faultModulePath = "github.com/zchee/agentctl"

type faultSourceFile struct {
	path  string
	file  *ast.File
	info  *types.Info
	pkg   *types.Package
	nodes map[ast.Decl][]ast.Node
}

type faultSourceInitializer struct {
	source *faultSourceFile
	expr   ast.Expr
}

type faultSourceIndex struct {
	files            map[string]*faultSourceFile
	functions        map[string]*types.Func
	initializers     map[types.Object]faultSourceInitializer
	initializerTypes map[types.Object]types.Type
}

type faultSourceImporter map[string]*types.Package

func (imports faultSourceImporter) Import(path string) (*types.Package, error) {
	if pkg := imports[path]; pkg != nil {
		return pkg, nil
	}
	return nil, fmt.Errorf("fixture import %q was not loaded", path)
}

func faultModuleRoot(t *testing.T) string {
	t.Helper()
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	root, err := faultModuleRootFrom(cwd)
	if err != nil {
		t.Fatal(err)
	}
	return root
}

// faultModuleRootFrom walks cwd's ancestors and validates the nearest go.mod.
// A nested module with another identity stops discovery rather than allowing
// the parent checkout to supply a different source-contract scope.
func faultModuleRootFrom(cwd string) (string, error) {
	for dir := cwd; ; {
		if info, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			if !info.IsDir() {
				path := filepath.Join(dir, "go.mod")
				data, err := os.ReadFile(path)
				if err != nil {
					return "", err
				}
				file, err := modfile.ParseLax(path, data, nil)
				if err != nil {
					return "", err
				}
				module := ""
				if file.Module != nil {
					module = file.Module.Mod.Path
				}
				if module != faultModulePath {
					return "", fmt.Errorf("wrong source-contract module: got %q, want %q", module, faultModulePath)
				}
				return dir, nil
			}
		} else if !os.IsNotExist(err) {
			return "", err
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return "", fmt.Errorf("cannot locate source-contract module go.mod from %s", cwd)
}

func loadFaultPackages(t *testing.T, tests bool, patterns ...string) []*packages.Package {
	t.Helper()
	cfg := &packages.Config{
		Context: t.Context(), Dir: faultModuleRoot(t), Tests: tests,
		Env:  append(os.Environ(), "GOWORK=off"),
		Mode: packages.NeedName | packages.NeedFiles | packages.NeedCompiledGoFiles | packages.NeedSyntax | packages.NeedTypes | packages.NeedTypesInfo | packages.NeedImports | packages.NeedDeps,
	}
	if faultSourceBuildTag != "" {
		cfg.BuildFlags = []string{"-tags=" + faultSourceBuildTag}
	}
	pkgs, err := packages.Load(cfg, patterns...)
	if err != nil {
		t.Fatal(err)
	}
	packages.Visit(pkgs, nil, func(pkg *packages.Package) {
		for _, err := range pkg.Errors {
			t.Errorf("load %s: %s", pkg.ID, err)
		}
	})
	if t.Failed() {
		t.Fatal("source-contract package load failed")
	}
	return pkgs
}

var faultFixtureImports struct {
	sync.Once
	imports faultSourceImporter
}

func loadFaultFixtureImports(t *testing.T) faultSourceImporter {
	t.Helper()
	faultFixtureImports.Do(func() {
		faultFixtureImports.imports = make(faultSourceImporter)
		packages.Visit(loadFaultPackages(t, false, faultImportPath, faultModulePath+"/internal/secret", faultModulePath+"/internal/provider/codex", "errors", "math"), nil, func(pkg *packages.Package) { faultFixtureImports.imports[pkg.PkgPath] = pkg.Types })
	})
	return faultFixtureImports.imports
}

func checkFaultFixture(t *testing.T, fset *token.FileSet, files map[string]*ast.File, imports faultSourceImporter) map[string]*faultSourceFile {
	t.Helper()
	groups := make(map[string][]*ast.File)
	for path, file := range files {
		groups[filepath.Dir(path)+":"+file.Name.Name] = append(groups[filepath.Dir(path)+":"+file.Name.Name], file)
	}
	sources := make(map[string]*faultSourceFile)
	for _, group := range groups {
		info := &types.Info{Types: make(map[ast.Expr]types.TypeAndValue), Defs: make(map[*ast.Ident]types.Object), Uses: make(map[*ast.Ident]types.Object), Selections: make(map[*ast.SelectorExpr]*types.Selection)}
		config := types.Config{Importer: imports, GoVersion: "go1.27", DisableUnusedImportCheck: true, Error: func(err error) { t.Errorf("fixture type check: %v", err) }}
		path := fset.PositionFor(group[0].Pos(), false).Filename
		pkg, err := config.Check(faultModulePath+"/"+filepath.ToSlash(filepath.Dir(path)), fset, group, info)
		if err != nil {
			t.Fatal("fixture must be valid Go")
		}
		for path, file := range files {
			for _, checked := range group {
				if file == checked {
					sources[path] = &faultSourceFile{path: path, file: file, info: info, pkg: pkg}
				}
			}
		}
	}
	return sources
}

func indexFaultSources(files map[string]*faultSourceFile) *faultSourceIndex {
	index := &faultSourceIndex{files: files, functions: make(map[string]*types.Func), initializers: make(map[types.Object]faultSourceInitializer), initializerTypes: make(map[types.Object]types.Type)}
	for _, source := range files {
		for _, object := range source.info.Defs {
			if fn, ok := object.(*types.Func); ok {
				index.functions[fn.Origin().FullName()] = fn.Origin()
			}
		}
	}
	for _, source := range files {
		source.nodes = make(map[ast.Decl][]ast.Node)
		for _, decl := range source.file.Decls {
			ast.Inspect(decl, func(node ast.Node) bool {
				switch node := node.(type) {
				case *ast.TypeSpec, *ast.SelectorExpr, *ast.CallExpr:
					source.nodes[decl] = append(source.nodes[decl], node)
				case *ast.AssignStmt:
					source.nodes[decl] = append(source.nodes[decl], node)
					if node.Tok == token.DEFINE {
						index.bindInitializers(source, node.Lhs, node.Rhs)
					}
				case *ast.ValueSpec:
					names := make([]ast.Expr, len(node.Names))
					for i, name := range node.Names {
						names[i] = name
					}
					index.bindInitializers(source, names, node.Values)
				}
				return true
			})
		}
	}
	return index
}

func (index *faultSourceIndex) bindInitializers(source *faultSourceFile, names, values []ast.Expr) {
	for i, expr := range names {
		name, ok := expr.(*ast.Ident)
		if !ok || source.info.Defs[name] == nil || len(values) == 0 {
			continue
		}
		object := source.info.Defs[name]
		if len(values) == len(names) {
			index.initializers[object] = faultSourceInitializer{source: source, expr: values[i]}
			index.initializerTypes[object] = source.info.TypeOf(values[i])
		} else if len(values) == 1 {
			if tuple, ok := source.info.TypeOf(values[0]).(*types.Tuple); ok && i < tuple.Len() {
				index.initializerTypes[object] = tuple.At(i).Type()
			}
		}
	}
}

func faultReceiver(typ types.Type) bool {
	typ = types.Unalias(typ)
	if pointer, ok := typ.(*types.Pointer); ok {
		typ = types.Unalias(pointer.Elem())
	}
	named, ok := typ.(*types.Named)
	return ok && named.Obj().Pkg() != nil && named.Obj().Pkg().Path() == faultImportPath && named.Obj().Name() == "Fault"
}

func faultPredicate(object types.Object) bool {
	fn, ok := object.(*types.Func)
	if !ok || fn.Pkg() == nil || fn.Pkg().Path() != faultImportPath {
		return false
	}
	sig := fn.Type().(*types.Signature)
	return sig.Recv() != nil && faultReceiver(sig.Recv().Type()) && (fn.Name() == "Is" || fn.Name() == "PausePoint" || fn.Name() == "WaitIf")
}

// Type information checks Fault, *Fault and embedded receivers. Interfaces and
// type parameters are outside the guard unless a declaration's initializer has
// static fault type; runtime provenance and reassignment are not inferred.
func (index *faultSourceIndex) callObject(source *faultSourceFile, expr ast.Expr) (types.Object, int) {
	seen := make(map[types.Object]bool)
	var resolve func(*faultSourceFile, ast.Expr) (types.Object, int)
	resolve = func(source *faultSourceFile, expr ast.Expr) (types.Object, int) {
		switch expr := ast.Unparen(expr).(type) {
		case *ast.IndexExpr:
			return resolve(source, expr.X)
		case *ast.IndexListExpr:
			return resolve(source, expr.X)
		case *ast.Ident:
			object := source.info.Uses[expr]
			if _, ok := object.(*types.Func); ok {
				return object, 0
			}
			if initializer := index.initializers[object]; !seen[object] && initializer.expr != nil {
				seen[object] = true
				return resolve(initializer.source, initializer.expr)
			}
		case *ast.SelectorExpr:
			if selection := source.info.Selections[expr]; selection != nil {
				object := selection.Obj()
				offset := 0
				if selection.Kind() == types.MethodExpr {
					offset = 1
				}
				if name, ok := ast.Unparen(expr.X).(*ast.Ident); ok && !faultPredicate(object) {
					if typ := index.initializerTypes[source.info.Uses[name]]; typ != nil {
						candidate, _, _ := types.LookupFieldOrMethod(typ, true, source.pkg, expr.Sel.Name)
						if faultPredicate(candidate) {
							object = candidate
						}
					}
				}
				return object, offset
			}
			return source.info.Uses[expr.Sel], 0
		}
		return nil, 0
	}
	object, offset := resolve(source, expr)
	// Test variants have distinct objects for the same declared function.
	if fn, ok := object.(*types.Func); ok {
		if canonical := index.functions[fn.Origin().FullName()]; canonical != nil {
			object = canonical
		}
	}
	return object, offset
}

func faultObjectReceiver(object types.Object, pkg, receiver, name string) bool {
	fn, ok := object.(*types.Func)
	if !ok || fn.Pkg() == nil || fn.Pkg().Path() != pkg || fn.Name() != name {
		return false
	}
	sig := fn.Type().(*types.Signature)
	if sig.Recv() == nil {
		return receiver == ""
	}
	typ := types.Unalias(sig.Recv().Type())
	if pointer, ok := typ.(*types.Pointer); ok {
		typ = types.Unalias(pointer.Elem())
	}
	named, ok := typ.(*types.Named)
	return ok && named.Obj().Name() == receiver
}

func faultHookField(object types.Object) bool {
	field, ok := object.(*types.Var)
	if !ok || !field.IsField() || field.Pkg() == nil || field.Pkg().Path() != faultModulePath+"/internal/secret" || (field.Name() != "Fault" && field.Name() != "Pause") {
		return false
	}
	typ := field.Pkg().Scope().Lookup("Seams")
	if typ == nil {
		return false
	}
	structure, ok := typ.Type().Underlying().(*types.Struct)
	if !ok {
		return false
	}
	for field := range structure.Fields() {
		if field == object {
			return true
		}
	}
	return false
}

func faultBaseSink(object types.Object) bool {
	return faultPredicate(object) || faultHookField(object) || faultObjectReceiver(object, faultModulePath+"/internal/secret", "Seams", "fault") || faultObjectReceiver(object, faultModulePath+"/internal/provider/codex", "", "abortRefreshFault")
}

func faultPackageSources(t *testing.T, pkgs []*packages.Package) (*token.FileSet, map[string]*faultSourceFile) {
	t.Helper()
	sources := make(map[string]*faultSourceFile)
	root := faultModuleRoot(t)
	var fset *token.FileSet
	foundFault := false
	for _, pkg := range pkgs {
		if pkg.PkgPath != faultModulePath && !strings.HasPrefix(pkg.PkgPath, faultModulePath+"/") {
			continue
		}
		foundFault = foundFault || pkg.PkgPath == faultImportPath
		fset = pkg.Fset
		for _, file := range pkg.Syntax {
			path, err := filepath.Rel(root, pkg.Fset.PositionFor(file.Pos(), false).Filename)
			if err != nil {
				t.Fatal(err)
			}
			path = filepath.ToSlash(path)
			if strings.HasPrefix(path, "../") {
				continue
			}
			sources[pkg.ID+":"+path] = &faultSourceFile{path: path, file: file, info: pkg.TypesInfo, pkg: pkg.Types}
		}
	}
	if !foundFault {
		t.Fatalf("source-contract package scope is missing %s", faultImportPath)
	}
	return fset, sources
}

func TestFaultModuleRoot(t *testing.T) {
	tests := map[string]struct {
		module  string
		content string
		nested  bool
		missing bool
		want    string
	}{
		"violation: wrong module cwd":                            {module: "example.com/other", want: `wrong source-contract module: got "example.com/other", want "` + faultModulePath + `"`},
		"violation: prefix collision module cwd":                 {module: faultModulePath + "-other", want: `wrong source-contract module: got "` + faultModulePath + `-other", want "` + faultModulePath + `"`},
		"control: expected module cwd":                           {module: faultModulePath},
		"violation: missing module directive":                    {content: "go 1.27\n", want: `wrong source-contract module: got "", want "` + faultModulePath + `"`},
		"violation: invalid module syntax":                       {content: "module (\n", want: "syntax error"},
		"violation: nested wrong module stops discovery":         {module: "example.com/nested", nested: true, want: `wrong source-contract module: got "example.com/nested", want "` + faultModulePath + `"`},
		"violation: no ancestral module rejects source fallback": {missing: true, want: "cannot locate source-contract module go.mod from "},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			cwd := t.TempDir()
			if tt.nested {
				if err := os.WriteFile(filepath.Join(cwd, "go.mod"), []byte("module "+faultModulePath+"\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				cwd = filepath.Join(cwd, "nested")
				if err := os.Mkdir(cwd, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			if !tt.missing {
				content := tt.content
				if content == "" {
					content = "module " + tt.module + "\ngo 1.27\n"
				}
				if err := os.WriteFile(filepath.Join(cwd, "go.mod"), []byte(content), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			want := tt.want
			if tt.content == "module (\n" {
				path := filepath.Join(cwd, "go.mod")
				want = fmt.Sprintf("%s:2: syntax error (unterminated block started at %s:1:1)", path, path)
			}
			if tt.missing {
				want += cwd
			}
			root, err := faultModuleRootFrom(cwd)
			got := ""
			if err != nil {
				got = err.Error()
			}
			t.Logf("measured discovery error=%q; root=%q", got, root)
			if diff := gocmp.Diff(want, got); diff != "" {
				t.Errorf("module discovery error (-want +got):\n%s", diff)
			}
			wantRoot := cwd
			if tt.want != "" {
				wantRoot = ""
			}
			if diff := gocmp.Diff(wantRoot, root); diff != "" {
				t.Errorf("module root (-want +got):\n%s", diff)
			}
		})
	}
}

func TestFaultPackageVariants(t *testing.T) {
	imports := loadFaultFixtureImports(t)
	root := faultModuleRoot(t)
	tests := map[string]struct {
		production      string
		productionDecls string
		testOnly        string
		directive       string
		reverse         bool
		wantPath        string
		wantExpressions map[string][]string
	}{
		"violation: test method shadows production sink":          {production: `wrapper{}.Is("hidden")`, testOnly: `func (wrapper) Is(string) bool { return false }`, wantPath: "probe.go"},
		"violation: test method shadows production sink reversed": {production: `wrapper{}.Is("hidden")`, testOnly: `func (wrapper) Is(string) bool { return false }`, reverse: true, wantPath: "probe.go"},
		"violation: sink exists only in test variant":             {production: `wrapper{}.Is(fault.BeforeRename)`, testOnly: `func testProbe() { fault.Active().Is("hidden") }`, wantPath: "probe_test.go"},
		"violation: shared sink diagnostics deduplicated":         {production: `wrapper{}.Is("hidden")`, wantPath: "probe.go"},
		"control: variants agree on tagged constant":              {production: `wrapper{}.Is(fault.BeforeRename)`},
		"control: prefix collision package excluded":              {production: `wrapper{}.Is("hidden")`},
		"violation: outside-root line directive retains caller": {
			production: `fault.Active().Is("hidden")`, directive: "/private/tmp/outside-module/generated.go", wantPath: "probe.go",
		},
		"violation: exempt-file line directive retains caller": {
			production: `fault.Active().Is("hidden")`, directive: filepath.Join(root, "internal/runtime/fault/fault_testing.go"), wantPath: "probe.go",
		},
		"violation: identical adjusted locations retain physical callers": {
			production: `fault.Active().Is("hidden")`, testOnly: "\nfunc other() { fault.Active().Is(\"hidden\") }",
			directive:       filepath.Join(root, "internal/commands/remapped.go"),
			wantExpressions: map[string][]string{"probe.go": {`"hidden"`}, "probe_test.go": {`"hidden"`}},
		},
		"violation: production helper called from test": {
			productionDecls: `func Forward(name string) { fault.Active().Is(name) }`, testOnly: `func testProbe() { Forward("hidden") }`,
			wantExpressions: map[string][]string{"probe.go": {"name)"}, "probe_test.go": {`"hidden"`}},
		},
		"violation: production helper called from test reversed": {
			productionDecls: `func Forward(name string) { fault.Active().Is(name) }`, testOnly: `func testProbe() { Forward("hidden") }`, reverse: true,
			wantExpressions: map[string][]string{"probe.go": {"name)"}, "probe_test.go": {`"hidden"`}},
		},
		"control: production helper test caller tagged constant": {
			productionDecls: `func Forward(name string) { fault.Active().Is(name) }`, testOnly: `func testProbe() { Forward(fault.BeforeRename) }`,
			wantExpressions: map[string][]string{"probe.go": {"name)"}},
		},
		"control: production helper test caller tagged constant reversed": {
			productionDecls: `func Forward(name string) { fault.Active().Is(name) }`, testOnly: `func testProbe() { Forward(fault.BeforeRename) }`, reverse: true,
			wantExpressions: map[string][]string{"probe.go": {"name)"}},
		},
		"violation: test helper called from test": {
			testOnly:        `func TestForward(name string) { fault.Active().Is(name) }; func testProbe() { TestForward("hidden") }`,
			wantExpressions: map[string][]string{"probe_test.go": {"name)", `"hidden"`}},
		},
		"violation: test helper called from test reversed": {
			testOnly: `func TestForward(name string) { fault.Active().Is(name) }; func testProbe() { TestForward("hidden") }`, reverse: true,
			wantExpressions: map[string][]string{"probe_test.go": {"name)", `"hidden"`}},
		},
		"control: test helper caller tagged constant": {
			testOnly:        `func TestForward(name string) { fault.Active().Is(name) }; func testProbe() { TestForward(fault.BeforeRename) }`,
			wantExpressions: map[string][]string{"probe_test.go": {"name)"}},
		},
		"control: test helper caller tagged constant reversed": {
			testOnly: `func TestForward(name string) { fault.Active().Is(name) }; func testProbe() { TestForward(fault.BeforeRename) }`, reverse: true,
			wantExpressions: map[string][]string{"probe_test.go": {"name)"}},
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			fset := token.NewFileSet()
			texts := map[string]string{
				"internal/commands/probe.go":      "package commands\nimport \"" + faultImportPath + "\"\ntype wrapper struct{ fault.Fault }\nfunc probe() { " + tt.production + " }\n" + tt.productionDecls,
				"internal/commands/probe_test.go": "package commands\nimport \"" + faultImportPath + "\"\n" + tt.testOnly,
			}
			if tt.directive != "" {
				for path, text := range texts {
					texts[path] = "//line " + tt.directive + ":1:1\n" + text
				}
			}
			files := make(map[string]*ast.File)
			for path, text := range texts {
				file, err := parser.ParseFile(fset, filepath.Join(root, path), text, parser.SkipObjectResolution)
				if err != nil {
					t.Fatal(err)
				}
				files[path] = file
			}
			const path = "internal/commands/probe.go"
			normalFile, err := parser.ParseFile(fset, filepath.Join(root, path), texts[path], parser.SkipObjectResolution)
			if err != nil {
				t.Fatal(err)
			}
			normal := checkFaultFixture(t, fset, map[string]*ast.File{path: normalFile}, imports)
			variant := checkFaultFixture(t, fset, files, imports)
			pkgPath := faultModulePath + "/internal/commands"
			if name == "control: prefix collision package excluded" {
				pkgPath = faultModulePath + "-other/internal/commands"
			}
			pkgs := []*packages.Package{
				{ID: pkgPath, PkgPath: pkgPath, Fset: fset, Syntax: []*ast.File{normalFile}, TypesInfo: normal[path].info, Types: normal[path].pkg},
				{ID: pkgPath + " [commands.test]", PkgPath: pkgPath, Fset: fset, Syntax: []*ast.File{files[path], files["internal/commands/probe_test.go"]}, TypesInfo: variant[path].info, Types: variant[path].pkg},
				{ID: faultImportPath, PkgPath: faultImportPath, Fset: fset},
			}
			if tt.reverse {
				pkgs[0], pkgs[1] = pkgs[1], pkgs[0]
			}
			loadedFset, sources := faultPackageSources(t, pkgs)
			got := faultSourceViolations(loadedFset, sources)
			var want []string
			if tt.wantPath != "" {
				path := "internal/commands/" + tt.wantPath
				pos := fset.File(files[path].Pos()).Pos(strings.Index(texts[path], `"hidden"`))
				want = []string{fmt.Sprintf("%s: fault name must reference a tagged fault constant, got %s", fset.PositionFor(pos, false), `"hidden"`)}
			}
			for _, path := range []string{"probe.go", "probe_test.go"} {
				for _, expr := range tt.wantExpressions[path] {
					path := "internal/commands/" + path
					pos := fset.File(files[path].Pos()).Pos(strings.Index(texts[path], expr))
					want = append(want, fmt.Sprintf("%s: fault name must reference a tagged fault constant, got %s", fset.PositionFor(pos, false), strings.TrimSuffix(expr, ")")))
				}
			}
			t.Logf("measured violations=%d; diagnostics=%v", len(got), got)
			if diff := gocmp.Diff(want, got); diff != "" {
				t.Errorf("variant diagnostics (-want +got):\n%s", diff)
			}
		})
	}
}
