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
	"bytes"
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	gocmp "github.com/google/go-cmp/cmp"
)

const faultImportPath = "github.com/zchee/agentctl/internal/runtime/fault"

func TestFaultSourceContract(t *testing.T) {
	root, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(root, "go.mod")); err == nil {
			break
		} else if !os.IsNotExist(err) {
			t.Fatal(err)
		}
		parent := filepath.Dir(root)
		if parent == root {
			t.Fatal("cannot locate module root containing go.mod")
		}
		root = parent
	}

	fset := token.NewFileSet()
	files := make(map[string]*ast.File)
	err = fs.WalkDir(os.DirFS(root), ".", func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if entry.Name() == ".git" || entry.Name() == "vendor" || entry.Name() == "testdata" {
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		file, err := parser.ParseFile(fset, path, mustReadSource(t, filepath.Join(root, path)), parser.ParseComments)
		if err != nil {
			return err
		}
		files[path] = file
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	testingNames := declaredFaultNames(t, fset, files["internal/runtime/fault/fault_testing.go"], false)
	releaseNames := declaredFaultNames(t, fset, files["internal/runtime/fault/fault_release.go"], true)
	if diff := gocmp.Diff(slices.Sorted(maps.Keys(testingNames)), slices.Sorted(maps.Keys(releaseNames))); diff != "" {
		t.Errorf("tagged fault constant names differ (-testing +release):\n%s", diff)
	}
	for _, violation := range faultSourceViolations(fset, files, testingNames) {
		t.Error(violation)
	}
}

func mustReadSource(t *testing.T, path string) []byte {
	t.Helper()
	source, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return source
}

func declaredFaultNames(t *testing.T, fset *token.FileSet, file *ast.File, release bool) map[string]bool {
	t.Helper()
	names := make(map[string]bool)
	if file == nil {
		t.Fatal("missing tagged fault constant file")
	}
	for _, decl := range file.Decls {
		block, ok := decl.(*ast.GenDecl)
		if !ok || block.Tok != token.CONST {
			continue
		}
		for _, spec := range block.Specs {
			value := spec.(*ast.ValueSpec)
			for i, name := range value.Names {
				if !name.IsExported() {
					continue
				}
				names[name.Name] = true
				if i >= len(value.Values) {
					t.Errorf("%s: %s must have an explicit string literal", fset.Position(name.Pos()), name.Name)
					continue
				}
				literal, ok := value.Values[i].(*ast.BasicLit)
				if !ok || literal.Kind != token.STRING {
					t.Errorf("%s: %s must be a string literal, got %s", fset.Position(name.Pos()), name.Name, sourceExpression(fset, value.Values[i]))
					continue
				}
				text, err := strconv.Unquote(literal.Value)
				if err != nil {
					t.Fatal(err)
				}
				if release {
					if text != "" {
						t.Errorf("%s: release %s must be empty, got %q", fset.Position(name.Pos()), name.Name, text)
					}
				} else if !validFaultStem(text) || literal.Value != strconv.Quote(text) {
					t.Errorf("%s: testing %s must be a plain lowercase [a-z][a-z0-9_]* literal, got %s", fset.Position(name.Pos()), name.Name, literal.Value)
				}
			}
		}
	}
	if len(names) == 0 {
		t.Error("tagged fault file has no exported fault constants")
	}
	return names
}

func validFaultStem(name string) bool {
	if len(name) == 0 || name[0] < 'a' || name[0] > 'z' {
		return false
	}
	for _, char := range name {
		if (char < 'a' || char > 'z') && (char < '0' || char > '9') && char != '_' {
			return false
		}
	}
	return true
}

func sourceExpression(fset *token.FileSet, node ast.Node) string {
	var text bytes.Buffer
	if err := format.Node(&text, fset, node); err != nil {
		return fmt.Sprintf("<cannot format %T: %v>", node, err)
	}
	return text.String()
}

func sourceImports(file *ast.File) map[string]string {
	imports := make(map[string]string)
	for _, spec := range file.Imports {
		path, err := strconv.Unquote(spec.Path.Value)
		if err != nil {
			continue // The parser has already rejected invalid string syntax.
		}
		name := filepath.Base(path)
		if spec.Name != nil {
			name = spec.Name.Name
		}
		imports[name] = path
	}
	return imports
}

func namedFaultConstant(fset *token.FileSet, file *ast.File, imports map[string]string, expr ast.Expr, names map[string]bool) bool {
	switch name := ast.Unparen(expr).(type) {
	case *ast.SelectorExpr:
		pkg, ok := name.X.(*ast.Ident)
		return ok && pkg.Obj == nil && imports[pkg.Name] == faultImportPath && names[name.Sel.Name]
	case *ast.Ident:
		if file.Name.Name != "fault" || !names[name.Name] {
			return false
		}
		// Identifiers declared in a different file have no parser object;
		// a local declaration must never shadow an approved constant name.
		if name.Obj == nil {
			return true
		}
		declaredIn := fset.Position(name.Obj.Pos()).Filename
		if name.Obj.Kind != ast.Con || (declaredIn != "internal/runtime/fault/fault_testing.go" && declaredIn != "internal/runtime/fault/fault_release.go") {
			return false
		}
		for _, decl := range file.Decls {
			if block, ok := decl.(*ast.GenDecl); ok && block.Tok == token.CONST {
				for _, spec := range block.Specs {
					if spec == name.Obj.Decl {
						return true
					}
				}
			}
		}
		return false
	default:
		return false
	}
}

// These are the existing generic forwarding boundaries, not injection
// declarations. New expressions inside the same functions still fail.
var faultNameForwarding = map[string]map[string]bool{
	"internal/secret/secret_file.go:SecretFile.WriteWithFaults": {
		"names.BeforeRename": true,
		"names.RenameFail":   true,
	},
	"internal/provider/codex/refresh_fault_testing.go:abortRefreshFault": {"name": true},
	"internal/secret/claude_lock_stale.go:Seams.fault":                   {"name": true},
	"internal/commands/refresh_keychain.go:Status.refreshMigrated":       {"seams.Fault = injected.Is": true},
	"internal/commands/use_live_write.go:useWrite":                       {"seams.Fault = injected.Is": true},
}

func faultFunctionName(fn *ast.FuncDecl) string {
	name := fn.Name.Name
	if fn.Recv != nil {
		typ := fn.Recv.List[0].Type
		if pointer, ok := typ.(*ast.StarExpr); ok {
			typ = pointer.X
		}
		if recv, ok := typ.(*ast.Ident); ok {
			name = recv.Name + "." + name
		}
	}
	return name
}

func (index *faultSourceIndex) faultCallArguments(source *faultSourceFile, call *ast.CallExpr, helpers map[faultSourceSymbol]map[int]bool) (faultSourceSymbol, []ast.Expr) {
	symbol := index.callSymbol(source, call.Fun)
	offset := 0
	if method, ok := ast.Unparen(call.Fun).(*ast.SelectorExpr); ok {
		receiver := ast.Unparen(method.X)
		if pointer, ok := receiver.(*ast.StarExpr); ok {
			receiver = ast.Unparen(pointer.X)
		}
		typ := index.typeSymbol(source, receiver)
		if index.typeDeclaration(source, typ).typ != nil || typ == (faultSourceSymbol{pkg: faultImportPath, name: "Fault"}) {
			offset = 1 // A resolved type selects a method expression, not a value.
		}
	}
	var args []ast.Expr
	for _, position := range slices.Sorted(maps.Keys(helpers[symbol])) {
		if position += offset; position < len(call.Args) {
			args = append(args, call.Args[position])
		}
	}
	return symbol, args
}

func discoverFaultHelpers(index *faultSourceIndex) map[faultSourceSymbol]map[int]bool {
	// Include helper call sites even when their names do not resemble a fault
	// API. A helper is found by forwarding a parameter to a known name sink.
	helpers := map[faultSourceSymbol]map[int]bool{
		{pkg: faultImportPath, receiver: "Fault", name: "Is"}:                          {0: true},
		{pkg: faultImportPath, receiver: "Fault", name: "PausePoint"}:                  {0: true},
		{pkg: faultImportPath, receiver: "Fault", name: "WaitIf"}:                      {0: true},
		{pkg: faultModulePath + "/internal/secret", receiver: "Seams", name: "fault"}:  {0: true},
		{pkg: faultModulePath + "/internal/secret", receiver: "Seams", name: "Fault"}:  {0: true},
		{pkg: faultModulePath + "/internal/secret", receiver: "Seams", name: "Pause"}:  {0: true},
		{pkg: faultModulePath + "/internal/provider/codex", name: "abortRefreshFault"}: {0: true},
	}
	for changed := true; changed; {
		changed = false
		for _, source := range index.files {
			file := source.file
			if file == nil {
				continue
			}
			for _, decl := range file.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if !ok || fn.Body == nil {
					continue
				}
				params := make(map[*ast.Object]int) //nolint:staticcheck // Parser objects are this syntactic guard's binding identity; it loads no type information.
				paramIndex := 0
				for _, field := range fn.Type.Params.List {
					for _, param := range field.Names {
						params[param.Obj] = paramIndex
						paramIndex++
					}
					if len(field.Names) == 0 {
						paramIndex++
					}
				}
				for _, node := range source.nodes[decl] {
					call, ok := node.(*ast.CallExpr)
					if !ok {
						continue
					}
					_, args := index.faultCallArguments(source, call, helpers)
					for _, arg := range args {
						param, ok := arg.(*ast.Ident)
						if !ok || param.Obj == nil {
							continue
						}
						position, ok := params[param.Obj]
						if !ok || helpers[source.functionSymbol(fn)][position] {
							continue
						}
						if helpers[source.functionSymbol(fn)] == nil {
							helpers[source.functionSymbol(fn)] = make(map[int]bool)
						}
						helpers[source.functionSymbol(fn)][position] = true
						changed = true
					}
				}
			}
		}
	}
	return helpers
}

func faultSourceViolations(fset *token.FileSet, files map[string]*ast.File, names map[string]bool) []string {
	index := indexFaultSources(files)
	helpers := discoverFaultHelpers(index)
	var violations []string
	for _, path := range slices.Sorted(maps.Keys(files)) {
		// The seam implementation and its parser/behaviour tests consume
		// arbitrary names by design. Other packages' tests are still checked.
		switch path {
		case "internal/runtime/fault/fault.go", "internal/runtime/fault/fault_release.go", "internal/runtime/fault/fault_testing.go",
			"internal/runtime/fault/fault_test.go", "internal/runtime/fault/fault_release_test.go", "internal/runtime/fault/fault_testing_test.go":
			continue
		}
		for _, decl := range files[path].Decls {
			violations = append(violations, index.declarationViolations(fset, path, decl, helpers, names)...)
		}
	}
	return violations
}

func (index *faultSourceIndex) declarationViolations(fset *token.FileSet, path string, decl ast.Decl, helpers map[faultSourceSymbol]map[int]bool, names map[string]bool) []string {
	var violations []string
	source := index.files[path]
	file, imports := source.file, source.imports
	function := ""
	if fn, ok := decl.(*ast.FuncDecl); ok {
		function = faultFunctionName(fn)
	}
	check := func(expr ast.Expr) {
		if faultNameForwarding[path+":"+function][sourceExpression(fset, expr)] {
			return
		}
		if !namedFaultConstant(fset, file, imports, expr, names) {
			violations = append(violations, fmt.Sprintf("%s: fault name must reference a tagged fault constant, got %s", fset.Position(expr.Pos()), sourceExpression(fset, expr)))
		}
	}
	checkedLiterals := make(map[*ast.CompositeLit]bool)
	calledMethods := make(map[*ast.SelectorExpr]bool)
	checkCarrier := func(literal *ast.CompositeLit) {
		checkedLiterals[literal] = true
		fields := make(map[string]ast.Expr)
		for _, field := range literal.Elts {
			keyed, ok := field.(*ast.KeyValueExpr)
			if !ok {
				continue
			}
			key, ok := keyed.Key.(*ast.Ident)
			if ok {
				fields[key.Name] = keyed.Value
			}
		}
		if len(literal.Elts) != 2 || len(fields) != 2 || fields["BeforeRename"] == nil || fields["RenameFail"] == nil {
			violations = append(violations, fmt.Sprintf("%s: WriteWithFaults carrier must have exactly BeforeRename and RenameFail keys, got %s", fset.Position(literal.Pos()), sourceExpression(fset, literal)))
			return
		}
		check(fields["BeforeRename"])
		check(fields["RenameFail"])
	}
	for _, node := range source.nodes[decl] {
		switch node := node.(type) {
		case *ast.ImportSpec:
			if node.Name != nil && node.Name.Name == "." {
				importPath, err := strconv.Unquote(node.Path.Value)
				if err == nil && importPath == faultImportPath {
					violations = append(violations, fmt.Sprintf("%s: fault points must be spelled through a qualified fault.<Const> selector; dot imports are forbidden", fset.Position(node.Pos())))
				}
			}
		case *ast.TypeSpec:
			if filepath.Dir(path) != "internal/secret" {
				name := ""
				switch typ := ast.Unparen(node.Type).(type) {
				case *ast.Ident:
					name = typ.Name
				case *ast.SelectorExpr:
					name = typ.Sel.Name
				}
				if strings.HasSuffix(name, "WriteFaultNames") {
					violations = append(violations, fmt.Sprintf("%s: fault-name carrier aliases and defined types are forbidden, got %s", fset.Position(node.Type.Pos()), sourceExpression(fset, node.Type)))
				}
			}
		case *ast.AssignStmt:
			if faultNameForwarding[path+":"+function][sourceExpression(fset, node)] {
				for _, rhs := range node.Rhs {
					if method, ok := ast.Unparen(rhs).(*ast.SelectorExpr); ok {
						calledMethods[method] = true // The two existing predicate hookups.
					}
				}
			}
		case *ast.SelectorExpr:
			symbol := index.callSymbol(source, node)
			if symbol.pkg == faultImportPath && symbol.receiver == "Fault" && (symbol.name == "Is" || symbol.name == "PausePoint" || symbol.name == "WaitIf") && !calledMethods[node] {
				violations = append(violations, fmt.Sprintf("%s: fault method must be called directly, not captured or assigned, got %s", fset.Position(node.Pos()), sourceExpression(fset, node)))
			}
		case *ast.CallExpr:
			if method, ok := ast.Unparen(node.Fun).(*ast.SelectorExpr); ok {
				calledMethods[method] = true
			}
			symbol, args := index.faultCallArguments(source, node, helpers)
			for _, arg := range args {
				check(arg)
			}
			if symbol == (faultSourceSymbol{pkg: faultModulePath + "/internal/secret", receiver: "SecretFile", name: "WriteWithFaults"}) && len(node.Args) > 0 {
				namesArg := ast.Unparen(node.Args[len(node.Args)-1])
				if literal, ok := namesArg.(*ast.CompositeLit); ok {
					checkCarrier(literal)
				} else {
					check(namesArg)
				}
			}
		case *ast.CompositeLit:
			if checkedLiterals[node] {
				continue
			}
			name := ""
			switch typ := node.Type.(type) {
			case *ast.Ident:
				name = typ.Name
			case *ast.SelectorExpr:
				name = typ.Sel.Name
			}
			if name == "WriteFaultNames" {
				for _, field := range node.Elts {
					if keyed, ok := field.(*ast.KeyValueExpr); ok {
						check(keyed.Value)
					} else {
						check(field)
					}
				}
			}
		}
	}
	return violations
}

func TestFaultNameCallShapes(t *testing.T) {
	tests := map[string]struct {
		decls  string
		source string
		want   string
		count  int
		order  []string
	}{
		"control: nested parenthesized tagged constant": {source: `fault.Active().Is(((fault.BeforeRename)))`},
		"control: nested parenthesized carrier fields":  {source: `writer.WriteWithFaults(ctx, doc, nil, stop, secret.WriteFaultNames{BeforeRename: ((fault.BeforeRename)), RenameFail: (((fault.BeforeRename)))})`},
		"control: parenthesized helper constant":        {source: `abortRefreshFault(((fault.BeforeRename)))`},
		"violation: discovered helper accepts parenthesized constant": {
			decls:  `func namedProbe(name string) { fault.Active().Is(name) }`,
			source: `namedProbe(((fault.BeforeRename)))`, want: `got name`,
		},
		"violation: parenthesized raw name":                 {source: `fault.Active().Is(("hidden"))`, want: `got ("hidden")`},
		"violation: parenthesized shadowed import constant": {source: `fault := struct{ BeforeRename string }{"hidden"}; injected.Is(((fault.BeforeRename)))`, want: `got (fault.BeforeRename)`},
		"control: tagged selector":                          {source: `fault.Active().Is(fault.BeforeRename)`},
		"violation: local variable":                         {source: `point := "hidden"; fault.Active().Is(point)`, want: "got point"},
		"violation: typed constant":                         {source: `const point string = "hidden"; fault.Active().Is(point)`, want: "got point"},
		"violation: string literal":                         {source: `fault.Active().Is("hidden")`, want: `got "hidden"`},
		"violation: concatenation":                          {source: `fault.Active().PausePoint("before_" + "rename")`, want: `got "before_" + "rename"`},
		"violation: local selector":                         {source: `fault := struct{ BeforeRename string }{"hidden"}; injected.WaitIf(fault.BeforeRename)`, want: "got fault.BeforeRename"},
		"violation: other package constant":                 {source: `fault.Active().Is(other.BeforeRename)`, want: "got other.BeforeRename"},
		"violation: unknown tagged name":                    {source: `fault.Active().Is(fault.Unknown)`, want: "got fault.Unknown"},
		"violation: writer literal":                         {source: `_ = secret.WriteFaultNames{BeforeRename: "hidden"}`, want: `got "hidden"`},
		"violation: helper variable":                        {source: `point := "hidden"; abortRefreshFault(point)`, want: "got point"},
		"control: method expression constant":               {source: `fault.Fault.Is(fault.Active(), fault.BeforeRename)`},
		"violation: method expression variable":             {source: `point := "hidden"; fault.Fault.Is(fault.Active(), point)`, want: "got point"},
		"violation: method value variable":                  {source: `check := fault.Active().Is; point := "hidden"; check(point)`, want: "got point", count: 2},
		"violation: method value with constant":             {source: `check := fault.Active().Is; check(fault.BeforeRename)`, want: "not captured or assigned"},
		"violation: reassigned method value":                {source: `var check func(string) bool; check = fault.Active().Is; check("hidden")`, want: "got fault.Active().Is"},
		"violation: assigned pause method":                  {source: `var check func(string); check = fault.Active().PausePoint; check("hidden")`, want: "got fault.Active().PausePoint"},
		"violation: assigned wait method":                   {source: `var check func(string); check = fault.Active().WaitIf; check("hidden")`, want: "got fault.Active().WaitIf"},
		"violation: parenthesized method capture":           {source: `var check func(string) bool; check = (fault.Active().Is)`, want: "got fault.Active().Is"},
		"violation: method value returned":                  {source: `return fault.Active().Is`, want: "got fault.Active().Is"},
		"violation: method value passed as argument":        {source: `consume(fault.Active().Is)`, want: "got fault.Active().Is"},
		"violation: named helper alias":                     {source: `check := abortRefreshFault; point := "hidden"; check(point)`, want: "got point"},
		"control: unrelated imported Is":                    {source: `errors.Is(err, nil)`},
		"violation: helper through alias method expression": {
			decls:  `type carrier = fault.Fault; func namedProbe(name string) { carrier.Is(fault.Active(), name) }`,
			source: `namedProbe("hidden")`, want: `got "hidden"`, count: 2,
		},
		"violation: helper through promoted method expression": {
			decls:  `type carrier struct{ fault.Fault }; func namedProbe(name string) { carrier.Is(carrier{}, name) }`,
			source: `namedProbe("hidden")`, want: `got "hidden"`, count: 2,
		},
		"violation: helper through instantiated promoted method expression": {
			decls:  `type carrier[T any] struct{ fault.Fault }; func namedProbe(receiver carrier[int], name string) { carrier[int].Is(receiver, name) }`,
			source: `namedProbe(carrier[int]{}, "hidden")`, want: `got "hidden"`, count: 2,
			order: []string{"got name", `got "hidden"`},
		},
		"violation: helper names retain argument order": {
			decls:  `func both(a, b string) { fault.Active().Is(a); fault.Active().Is(b) }`,
			source: `both("first", "second")`, want: `got "first"`, count: 4,
			order: []string{"got a", "got b", `got "first"`, `got "second"`},
		},
		"violation: derived helper": {
			decls:  `func namedProbe(name string) { fault.Active().Is(name) }`,
			source: `namedProbe("hidden")`, want: `got "hidden"`, count: 2,
		},
		"violation: derived helper chain": {
			decls:  `func namedProbe(name string) { fault.Active().Is(name) }; func outerProbe(name string) { namedProbe(name) }`,
			source: `outerProbe("hidden")`, want: `got "hidden"`, count: 3,
		},
		"violation: carrier alias":                   {decls: `type carrier = secret.WriteFaultNames`, source: `_ = carrier{BeforeRename: "hidden"}`, want: "got secret.WriteFaultNames"},
		"violation: carrier defined type":            {decls: `type carrier secret.WriteFaultNames`, source: `_ = carrier{BeforeRename: "hidden"}`, want: "got secret.WriteFaultNames"},
		"violation: bare carrier alias":              {decls: `type carrier = WriteFaultNames`, want: "got WriteFaultNames"},
		"violation: carrier name suffix":             {decls: `type carrier = other.CustomWriteFaultNames`, want: "got other.CustomWriteFaultNames"},
		"violation: carrier alias inside function":   {source: `type carrier = secret.WriteFaultNames; _ = carrier{}`, want: "got secret.WriteFaultNames"},
		"violation: disguised writer argument":       {decls: `type carrier = struct{BeforeRename, RenameFail string}`, source: `writer.WriteWithFaults(ctx, doc, nil, stop, carrier{BeforeRename: "hidden", RenameFail: fault.BeforeRename})`, want: `got "hidden"`},
		"control: disguised writer tagged arguments": {decls: `type carrier = struct{BeforeRename, RenameFail string}`, source: `writer.WriteWithFaults(ctx, doc, nil, stop, carrier{BeforeRename: fault.BeforeRename, RenameFail: fault.BeforeRename})`},
		"violation: writer missing field":            {source: `writer.WriteWithFaults(ctx, doc, nil, stop, carrier{BeforeRename: fault.BeforeRename})`, want: "exactly BeforeRename and RenameFail keys"},
		"violation: writer wrong field":              {source: `writer.WriteWithFaults(ctx, doc, nil, stop, carrier{BeforeRename: fault.BeforeRename, Other: fault.BeforeRename})`, want: "exactly BeforeRename and RenameFail keys"},
		"violation: writer positional fields":        {source: `writer.WriteWithFaults(ctx, doc, nil, stop, carrier{fault.BeforeRename, fault.BeforeRename})`, want: "exactly BeforeRename and RenameFail keys"},
		"violation: writer duplicate field":          {source: `writer.WriteWithFaults(ctx, doc, nil, stop, carrier{BeforeRename: fault.BeforeRename, BeforeRename: fault.BeforeRename})`, want: "exactly BeforeRename and RenameFail keys"},
		"control: unrelated function value":          {source: `check := errors.Is; check(err, nil)`},
		"violation: writer variable":                 {source: `var names secret.WriteFaultNames; writer.WriteWithFaults(ctx, doc, nil, stop, names)`, want: "got names"},
		"violation: unkeyed writer literal":          {source: `_ = secret.WriteFaultNames{"hidden", fault.BeforeRename}`, want: `got "hidden"`},
		"control: writer selectors":                  {source: `_ = secret.WriteFaultNames{BeforeRename: fault.BeforeRename, RenameFail: fault.BeforeRename}`},
		"control: parenthesized call":                {source: `(fault.Active().Is)(fault.BeforeRename)`},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			fset := token.NewFileSet()
			file, err := parser.ParseFile(fset, "fixture.go", "package codex\nimport (\""+faultImportPath+"\"; \"errors\"; \""+faultModulePath+"/internal/secret\")\nvar injected fault.Fault\nvar writer secret.SecretFile\n"+tt.decls+"\nfunc probe() {"+tt.source+"}", 0)
			if err != nil {
				t.Fatal(err)
			}
			got := faultSourceViolations(fset, map[string]*ast.File{"internal/provider/codex/fixture.go": file}, map[string]bool{"BeforeRename": true})
			t.Logf("measured violations=%d; diagnostics=%v", len(got), got)
			if tt.order != nil {
				var order []string
				for _, diagnostic := range got {
					_, suffix, _ := strings.Cut(diagnostic, ", ")
					order = append(order, suffix)
				}
				t.Logf("measured diagnostic order=%q", order)
				if diff := gocmp.Diff(tt.order, order); diff != "" {
					t.Errorf("diagnostic order (-want +got):\n%s\ndiagnostics: %v", diff, got)
				}
			}
			if tt.want == "" {
				if diff := gocmp.Diff([]string(nil), got); diff != "" {
					t.Errorf("violations (-want +got):\n%s", diff)
				}
			} else {
				if len(got) != max(tt.count, 1) || !strings.Contains(strings.Join(got, "\n"), tt.want) {
					t.Errorf("violations = %v, want %d diagnostics containing %q", got, max(tt.count, 1), tt.want)
				}
				for _, violation := range got {
					if !strings.Contains(violation, "fixture.go:") {
						t.Errorf("diagnostic lacks file:line: %s", violation)
					}
				}
			}
		})
	}
}
