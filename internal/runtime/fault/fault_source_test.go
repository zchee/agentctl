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
	"cmp"
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"go/types"
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
	root := faultModuleRoot(t)
	fset, sources := faultPackageSources(t, loadFaultPackages(t, true, "./..."))
	files := make(map[string]*ast.File)
	for _, path := range []string{"internal/runtime/fault/fault_testing.go", "internal/runtime/fault/fault_release.go"} {
		file, err := parser.ParseFile(fset, path, mustReadSource(t, filepath.Join(root, path)), parser.ParseComments|parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		files[path] = file
	}
	testingNames := declaredFaultNames(t, fset, files["internal/runtime/fault/fault_testing.go"], false)
	releaseNames := declaredFaultNames(t, fset, files["internal/runtime/fault/fault_release.go"], true)
	if diff := gocmp.Diff(slices.Sorted(maps.Keys(testingNames)), slices.Sorted(maps.Keys(releaseNames))); diff != "" {
		t.Errorf("tagged fault constant names differ (-testing +release):\n%s", diff)
	}
	for _, violation := range faultSourceViolations(fset, sources) {
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
					t.Errorf("%s: %s must have an explicit string literal", fset.PositionFor(name.Pos(), false), name.Name)
					continue
				}
				literal, ok := value.Values[i].(*ast.BasicLit)
				if !ok || literal.Kind != token.STRING {
					t.Errorf("%s: %s must be a string literal, got %s", fset.PositionFor(name.Pos(), false), name.Name, sourceExpression(fset, value.Values[i]))
					continue
				}
				text, err := strconv.Unquote(literal.Value)
				if err != nil {
					t.Fatal(err)
				}
				if release {
					if text != "" {
						t.Errorf("%s: release %s must be empty, got %q", fset.PositionFor(name.Pos(), false), name.Name, text)
					}
				} else if !validFaultStem(text) || literal.Value != strconv.Quote(text) {
					t.Errorf("%s: testing %s must be a plain lowercase [a-z][a-z0-9_]* literal, got %s", fset.PositionFor(name.Pos(), false), name.Name, literal.Value)
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

func namedFaultConstant(info *types.Info, expr ast.Expr) bool {
	selector, ok := ast.Unparen(expr).(*ast.SelectorExpr)
	if !ok {
		return false
	}
	name, ok := ast.Unparen(selector.X).(*ast.Ident)
	if !ok {
		return false
	}
	pkg, ok := info.Uses[name].(*types.PkgName)
	if !ok || pkg.Imported().Path() != faultImportPath {
		return false
	}
	constant, ok := info.Uses[selector.Sel].(*types.Const)
	return ok && constant.Pkg() != nil && constant.Pkg().Path() == faultImportPath
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

func (index *faultSourceIndex) faultCallArguments(source *faultSourceFile, call *ast.CallExpr, helpers map[types.Object]map[int]bool) (types.Object, []ast.Expr) {
	object, offset := index.callObject(source, call.Fun)
	positions := helpers[object]
	if faultBaseSink(object) {
		positions = map[int]bool{0: true}
	}
	if len(positions) > 0 && len(call.Args) == 1 {
		if _, ok := source.info.TypeOf(call.Args[0]).(*types.Tuple); ok {
			return object, call.Args
		}
	}
	var args []ast.Expr
	for _, position := range slices.Sorted(maps.Keys(positions)) {
		if position += offset; position < len(call.Args) {
			args = append(args, call.Args[position])
		}
	}
	return object, args
}

func discoverFaultHelpers(index *faultSourceIndex) map[types.Object]map[int]bool {
	helpers := make(map[types.Object]map[int]bool)
	for changed := true; changed; {
		changed = false
		for _, source := range index.files {
			for _, decl := range source.file.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if !ok || fn.Body == nil {
					continue
				}
				object, ok := source.info.Defs[fn.Name].(*types.Func)
				if !ok {
					continue
				}
				helper := index.functions[object.Origin().FullName()]
				params := make(map[types.Object]int)
				signature := object.Type().(*types.Signature)
				for i := range signature.Params().Len() {
					params[signature.Params().At(i)] = i
				}
				for _, node := range source.nodes[decl] {
					call, ok := node.(*ast.CallExpr)
					if !ok {
						continue
					}
					_, args := index.faultCallArguments(source, call, helpers)
					for _, arg := range args {
						param, ok := ast.Unparen(arg).(*ast.Ident)
						if !ok {
							continue
						}
						position, ok := params[source.info.Uses[param]]
						if !ok || helpers[helper][position] {
							continue
						}
						if helpers[helper] == nil {
							helpers[helper] = make(map[int]bool)
						}
						helpers[helper][position] = true
						changed = true
					}
				}
			}
		}
	}
	return helpers
}

type faultSourceDiagnostic struct {
	position token.Position
	message  string
}

func faultSourceViolations(fset *token.FileSet, files map[string]*faultSourceFile) []string {
	index := indexFaultSources(files)
	helpers := discoverFaultHelpers(index)
	var diagnostics []faultSourceDiagnostic
	for _, source := range files {
		// The seam implementation and its parser/behaviour tests consume
		// arbitrary names by design. Other packages' tests are still checked.
		switch source.path {
		case "internal/runtime/fault/fault.go", "internal/runtime/fault/fault_release.go", "internal/runtime/fault/fault_testing.go",
			"internal/runtime/fault/fault_test.go", "internal/runtime/fault/fault_release_test.go", "internal/runtime/fault/fault_testing_test.go":
			continue
		}
		for _, decl := range source.file.Decls {
			diagnostics = append(diagnostics, index.declarationViolations(fset, source, decl, helpers)...)
		}
	}
	slices.SortFunc(diagnostics, func(a, b faultSourceDiagnostic) int {
		return cmp.Or(cmp.Compare(a.position.Filename, b.position.Filename), cmp.Compare(a.position.Line, b.position.Line), cmp.Compare(a.position.Column, b.position.Column), cmp.Compare(a.message, b.message))
	})
	diagnostics = slices.CompactFunc(diagnostics, func(a, b faultSourceDiagnostic) bool {
		return a.position.Filename == b.position.Filename && a.position.Line == b.position.Line && a.position.Column == b.position.Column && a.message == b.message
	})
	var violations []string
	for _, diagnostic := range diagnostics {
		violations = append(violations, fmt.Sprintf("%s: %s", diagnostic.position, diagnostic.message))
	}
	return violations
}

func (index *faultSourceIndex) declarationViolations(fset *token.FileSet, source *faultSourceFile, decl ast.Decl, helpers map[types.Object]map[int]bool) []faultSourceDiagnostic {
	var violations []faultSourceDiagnostic
	path := source.path
	function := ""
	if fn, ok := decl.(*ast.FuncDecl); ok {
		function = faultFunctionName(fn)
	}
	check := func(expr ast.Expr) {
		if faultNameForwarding[path+":"+function][sourceExpression(fset, expr)] {
			return
		}
		if !namedFaultConstant(source.info, expr) {
			violations = append(violations, faultSourceDiagnostic{position: fset.PositionFor(expr.Pos(), false), message: fmt.Sprintf("fault name must reference a tagged fault constant, got %s", sourceExpression(fset, expr))})
		}
	}
	calledMethods := make(map[*ast.SelectorExpr]bool)
	checkCarrier := func(literal *ast.CompositeLit) {
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
			violations = append(violations, faultSourceDiagnostic{position: fset.PositionFor(literal.Pos(), false), message: fmt.Sprintf("WriteWithFaults carrier must have exactly BeforeRename and RenameFail keys, got %s", sourceExpression(fset, literal))})
			return
		}
		check(fields["BeforeRename"])
		check(fields["RenameFail"])
	}
	for _, node := range source.nodes[decl] {
		switch node := node.(type) {
		case *ast.TypeSpec:
			if filepath.Dir(path) != "internal/secret" {
				typ := source.info.TypeOf(node.Type)
				if typ != nil {
					if named, ok := types.Unalias(typ).(*types.Named); ok && named.Obj().Pkg() != nil && named.Obj().Pkg().Path() == faultModulePath+"/internal/secret" && named.Obj().Name() == "WriteFaultNames" {
						violations = append(violations, faultSourceDiagnostic{position: fset.PositionFor(node.Type.Pos(), false), message: fmt.Sprintf("fault-name carrier aliases and defined types are forbidden, got %s", sourceExpression(fset, node.Type))})
					}
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
			object, _ := index.callObject(source, node)
			if faultPredicate(object) && !calledMethods[node] {
				violations = append(violations, faultSourceDiagnostic{position: fset.PositionFor(node.Pos(), false), message: fmt.Sprintf("fault method must be called directly, not captured or assigned, got %s", sourceExpression(fset, node))})
			}
		case *ast.CallExpr:
			if method, ok := ast.Unparen(node.Fun).(*ast.SelectorExpr); ok {
				calledMethods[method] = true
			}
			symbol, args := index.faultCallArguments(source, node, helpers)
			for _, arg := range args {
				check(arg)
			}
			if faultObjectReceiver(symbol, faultModulePath+"/internal/secret", "SecretFile", "WriteWithFaults") && len(node.Args) > 0 {
				namesArg := ast.Unparen(node.Args[len(node.Args)-1])
				if literal, ok := namesArg.(*ast.CompositeLit); ok {
					checkCarrier(literal)
				} else {
					check(namesArg)
				}
			}
		}
	}
	return violations
}

func TestFaultNameCallShapes(t *testing.T) {
	imports := loadFaultFixtureImports(t)
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
		"violation: other package constant":                 {source: `fault.Active().Is(secret.ClaudeProcessName)`, want: "got secret.ClaudeProcessName"},
		"violation: writer literal":                         {source: `writer.WriteWithFaults(ctx, doc, nil, stop, secret.WriteFaultNames{BeforeRename: "hidden", RenameFail: fault.RenameFail})`, want: `got "hidden"`},
		"violation: reversed carrier fields follow source order": {
			source: `writer.WriteWithFaults(ctx, doc, nil, stop, secret.WriteFaultNames{RenameFail: "first", BeforeRename: "second"})`, want: `got "first"`, count: 2,
			order: []string{`got "first"`, `got "second"`},
		},
		"violation: helper variable":            {source: `point := "hidden"; abortRefreshFault(point)`, want: "got point"},
		"control: method expression constant":   {source: `fault.Fault.Is(fault.Active(), fault.BeforeRename)`},
		"violation: method expression variable": {source: `point := "hidden"; fault.Fault.Is(fault.Active(), point)`, want: "got point"},
		"violation: tuple-expanded value method expression": {
			decls:  `func pair() (fault.Fault, string) { return fault.Active(), "hidden" }`,
			source: `fault.Fault.Is(pair())`, want: "fault name must reference a tagged fault constant, got pair()",
		},
		"violation: tuple-expanded pointer method expression": {
			decls:  `func pairPtr() (*fault.Fault, string) { return new(fault.Active()), "hidden" }`,
			source: `(*fault.Fault).Is(pairPtr())`, want: "fault name must reference a tagged fault constant, got pairPtr()",
		},
		"violation: tuple-expanded pause method expression": {
			decls:  `func pair() (fault.Fault, string) { return fault.Active(), "hidden" }`,
			source: `fault.Fault.PausePoint(pair())`, want: "fault name must reference a tagged fault constant, got pair()",
		},
		"violation: tuple-expanded wait method expression": {
			decls:  `func pair() (fault.Fault, string) { return fault.Active(), "hidden" }`,
			source: `fault.Fault.WaitIf(pair())`, want: "fault name must reference a tagged fault constant, got pair()",
		},
		"violation: tuple-expanded promoted method expression": {
			decls:  `type wrapper struct{ fault.Fault }; func pair() (wrapper, string) { return wrapper{}, "hidden" }`,
			source: `wrapper.Is(pair())`, want: "fault name must reference a tagged fault constant, got pair()",
		},
		"violation: tuple-expanded tagged constant requires call-site selector": {
			decls:  `func pair() (fault.Fault, string) { return fault.Active(), fault.BeforeRename }`,
			source: `fault.Fault.Is(pair())`, want: "fault name must reference a tagged fault constant, got pair()",
		},
		"violation: tuple-expanded discovered helper": {
			decls:  `func pair() (fault.Fault, string) { return fault.Active(), "hidden" }; func namedProbe(receiver fault.Fault, name string) { receiver.Is(name) }`,
			source: `namedProbe(pair())`, want: "fault name must reference a tagged fault constant, got pair()", count: 2,
			order: []string{"got name", "got pair()"},
		},
		"violation: tuple-expanded writer carrier": {
			decls:  `func writeArgs() (context.Context, []byte, *secret.PendingSpec, secret.StopPolicy, secret.WriteFaultNames) { return ctx, doc, nil, stop, secret.WriteFaultNames{BeforeRename: "hidden", RenameFail: fault.RenameFail} }`,
			source: `writer.WriteWithFaults(writeArgs())`, want: "fault name must reference a tagged fault constant, got writeArgs()",
		},
		"violation: tuple-expanded writer method expression carrier": {
			decls:  `func writeArgs() (*secret.SecretFile, context.Context, []byte, *secret.PendingSpec, secret.StopPolicy, secret.WriteFaultNames) { return &writer, ctx, doc, nil, stop, secret.WriteFaultNames{BeforeRename: "hidden", RenameFail: fault.RenameFail} }`,
			source: `(*secret.SecretFile).WriteWithFaults(writeArgs())`, want: "fault name must reference a tagged fault constant, got writeArgs()",
		},
		"control: tuple-expanded unrelated method expression": {
			decls:  `type predicate struct{}; func (predicate) Is(string) bool { return false }; func pair() (predicate, string) { return predicate{}, "ordinary" }`,
			source: `predicate.Is(pair())`,
		},
		"violation: method value variable":           {source: `check := fault.Active().Is; point := "hidden"; check(point)`, want: "got point", count: 2},
		"violation: method value with constant":      {source: `check := fault.Active().Is; check(fault.BeforeRename)`, want: "not captured or assigned"},
		"violation: reassigned method value":         {source: `var check func(string) bool; check = fault.Active().Is; check("hidden")`, want: "got fault.Active().Is"},
		"violation: assigned pause method":           {source: `var check func(string); check = fault.Active().PausePoint; check("hidden")`, want: "got fault.Active().PausePoint"},
		"violation: assigned wait method":            {source: `var check func(string); check = fault.Active().WaitIf; check("hidden")`, want: "got fault.Active().WaitIf"},
		"violation: parenthesized method capture":    {source: `var check func(string) bool; check = (fault.Active().Is); _ = check`, want: "got fault.Active().Is"},
		"violation: method value returned":           {decls: `func capture() func(string) bool { return fault.Active().Is }`, want: "got fault.Active().Is"},
		"violation: method value passed as argument": {source: `consume(fault.Active().Is)`, want: "got fault.Active().Is"},
		"violation: named helper alias":              {source: `check := abortRefreshFault; point := "hidden"; check(point)`, want: "got point"},
		"control: unrelated imported Is":             {source: `errors.Is(err, nil)`},
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
		"violation: carrier alias inside function":   {source: `type carrier = secret.WriteFaultNames; _ = carrier{}`, want: "got secret.WriteFaultNames"},
		"violation: disguised writer argument":       {decls: `type carrier = struct{BeforeRename, RenameFail string}`, source: `writer.WriteWithFaults(ctx, doc, nil, stop, carrier{BeforeRename: "hidden", RenameFail: fault.BeforeRename})`, want: `got "hidden"`},
		"control: disguised writer tagged arguments": {decls: `type carrier = struct{BeforeRename, RenameFail string}`, source: `writer.WriteWithFaults(ctx, doc, nil, stop, carrier{BeforeRename: fault.BeforeRename, RenameFail: fault.BeforeRename})`},
		"violation: writer missing field":            {source: `writer.WriteWithFaults(ctx, doc, nil, stop, secret.WriteFaultNames{BeforeRename: fault.BeforeRename})`, want: "exactly BeforeRename and RenameFail keys"},
		"violation: writer positional fields":        {source: `writer.WriteWithFaults(ctx, doc, nil, stop, secret.WriteFaultNames{fault.BeforeRename, fault.BeforeRename})`, want: "exactly BeforeRename and RenameFail keys"},
		"control: unrelated function value":          {source: `check := errors.Is; check(err, nil)`},
		"violation: writer variable":                 {source: `var names secret.WriteFaultNames; writer.WriteWithFaults(ctx, doc, nil, stop, names)`, want: "got names"},
		"violation: unkeyed writer literal":          {source: `writer.WriteWithFaults(ctx, doc, nil, stop, secret.WriteFaultNames{"hidden", fault.BeforeRename})`, want: "exactly BeforeRename and RenameFail keys"},
		"control: writer selectors":                  {source: `writer.WriteWithFaults(ctx, doc, nil, stop, secret.WriteFaultNames{BeforeRename: fault.BeforeRename, RenameFail: fault.BeforeRename})`},
		"control: unused carrier is not a sink":      {source: `_ = secret.WriteFaultNames{BeforeRename: "ordinary"}`},
		"control: parenthesized call":                {source: `(fault.Active().Is)(fault.BeforeRename)`},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			fset := token.NewFileSet()
			file, err := parser.ParseFile(fset, "internal/provider/codex/fixture.go", "package codex\nimport (\""+faultImportPath+"\"; \"errors\"; \"context\"; \""+faultModulePath+"/internal/secret\")\nvar injected fault.Fault\nvar writer secret.SecretFile\nvar ctx context.Context\nvar doc []byte\nvar stop = secret.StopComplete\nvar err error\nfunc consume(check func(string) bool) { _ = check }\n"+tt.decls+"\nfunc probe() {"+tt.source+"}", parser.SkipObjectResolution)
			if err != nil {
				t.Fatal(err)
			}
			helper, err := parser.ParseFile(fset, "internal/provider/codex/refresh_fault_testing.go", `package codex; import "`+faultImportPath+`"; func abortRefreshFault(name string) { fault.Active().Is(name) }`, parser.SkipObjectResolution)
			if err != nil {
				t.Fatal(err)
			}
			files := map[string]*ast.File{"internal/provider/codex/fixture.go": file, "internal/provider/codex/refresh_fault_testing.go": helper}
			got := faultSourceViolations(fset, checkFaultFixture(t, fset, files, imports))
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
