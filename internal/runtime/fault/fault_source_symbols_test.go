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
	"go/ast"
	"go/token"
	"path/filepath"
	"strings"
)

const faultModulePath = "github.com/zchee/agentctl"

// Symbols use nominal receiver names, not method spelling alone. This keeps
// unrelated Is methods and same-named forwarding functions out of the inventory.
type faultSourceSymbol struct {
	pkg, receiver, name string
	local               *ast.TypeSpec
}

type faultSourceFile struct {
	file    *ast.File
	pkg     string
	imports map[string]string
	nodes   map[ast.Decl][]ast.Node
}

type faultSourceDeclaration struct {
	source *faultSourceFile
	fn     *ast.FuncDecl
	typ    *ast.TypeSpec
	value  *ast.ValueSpec
}

type faultSourceType struct {
	source *faultSourceFile
	expr   ast.Expr
}

type faultSourceIndex struct {
	files     map[string]*faultSourceFile
	functions map[faultSourceSymbol]faultSourceDeclaration
	types     map[faultSourceSymbol]faultSourceDeclaration
	variables map[faultSourceSymbol]faultSourceDeclaration
}

func indexFaultSources(files map[string]*ast.File) *faultSourceIndex {
	index := &faultSourceIndex{
		files:     make(map[string]*faultSourceFile),
		functions: make(map[faultSourceSymbol]faultSourceDeclaration),
		types:     make(map[faultSourceSymbol]faultSourceDeclaration),
		variables: make(map[faultSourceSymbol]faultSourceDeclaration),
	}
	for path, file := range files {
		pkg := faultModulePath
		if dir := filepath.ToSlash(filepath.Dir(path)); dir != "." {
			pkg += "/" + dir
		}
		if strings.HasSuffix(file.Name.Name, "_test") {
			pkg += "_test"
		}
		source := &faultSourceFile{file: file, pkg: pkg, imports: sourceImports(file), nodes: make(map[ast.Decl][]ast.Node)}
		index.files[path] = source
		for _, decl := range file.Decls {
			// Cache preorder once so discovery and validation share capture ordering.
			ast.Inspect(decl, func(node ast.Node) bool {
				switch node.(type) {
				case *ast.TypeSpec, *ast.ImportSpec, *ast.AssignStmt, *ast.SelectorExpr, *ast.CallExpr, *ast.CompositeLit:
					source.nodes[decl] = append(source.nodes[decl], node)
				}
				return true
			})
			switch decl := decl.(type) {
			case *ast.FuncDecl:
				index.functions[source.functionSymbol(decl)] = faultSourceDeclaration{source: source, fn: decl}
			case *ast.GenDecl:
				for _, spec := range decl.Specs {
					if typ, ok := spec.(*ast.TypeSpec); ok {
						index.types[faultSourceSymbol{pkg: pkg, name: typ.Name.Name}] = faultSourceDeclaration{source: source, typ: typ}
					}
					if value, ok := spec.(*ast.ValueSpec); ok && decl.Tok == token.VAR {
						for _, name := range value.Names {
							index.variables[faultSourceSymbol{pkg: pkg, name: name.Name}] = faultSourceDeclaration{source: source, value: value}
						}
					}
				}
			}
		}
	}
	return index
}

func (source *faultSourceFile) functionSymbol(fn *ast.FuncDecl) faultSourceSymbol {
	symbol := faultSourceSymbol{pkg: source.pkg, name: fn.Name.Name}
	if fn.Recv != nil {
		receiver := ast.Unparen(fn.Recv.List[0].Type)
		for {
			switch typ := receiver.(type) {
			case *ast.StarExpr:
				receiver = ast.Unparen(typ.X)
			case *ast.IndexExpr:
				receiver = ast.Unparen(typ.X)
			case *ast.IndexListExpr:
				receiver = ast.Unparen(typ.X)
			default:
				if name, ok := receiver.(*ast.Ident); ok {
					symbol.receiver = name.Name
				}
				return symbol
			}
		}
	}
	return symbol
}

func (index *faultSourceIndex) typeSymbol(source *faultSourceFile, expr ast.Expr) faultSourceSymbol {
	seen := make(map[faultSourceSymbol]bool)
	var resolve func(*faultSourceFile, ast.Expr) faultSourceSymbol
	resolve = func(source *faultSourceFile, expr ast.Expr) faultSourceSymbol {
		if expr == nil {
			return faultSourceSymbol{}
		}
		var symbol faultSourceSymbol
		switch typ := ast.Unparen(expr).(type) {
		case *ast.StarExpr:
			return resolve(source, typ.X)
		case *ast.IndexExpr:
			return resolve(source, typ.X)
		case *ast.IndexListExpr:
			return resolve(source, typ.X)
		case *ast.Ident:
			symbol = faultSourceSymbol{pkg: source.pkg, name: typ.Name}
			if typ.Obj != nil {
				if decl, ok := typ.Obj.Decl.(*ast.TypeSpec); ok {
					if index.types[symbol].typ != decl {
						symbol.local = decl
					}
				} else {
					return faultSourceSymbol{}
				}
			}
		case *ast.SelectorExpr:
			if pkg, ok := typ.X.(*ast.Ident); ok && pkg.Obj == nil && source.imports[pkg.Name] != "" {
				symbol = faultSourceSymbol{pkg: source.imports[pkg.Name], name: typ.Sel.Name}
			}
		}
		if seen[symbol] {
			return faultSourceSymbol{}
		}
		seen[symbol] = true
		decl := index.typeDeclaration(source, symbol)
		if decl.typ != nil && decl.typ.Assign.IsValid() {
			if _, structure := ast.Unparen(decl.typ.Type).(*ast.StructType); !structure {
				return resolve(decl.source, decl.typ.Type)
			}
		}
		return symbol
	}
	return resolve(source, expr)
}

func (index *faultSourceIndex) typeDeclaration(source *faultSourceFile, symbol faultSourceSymbol) faultSourceDeclaration {
	if symbol.local != nil {
		return faultSourceDeclaration{source: source, typ: symbol.local}
	}
	return index.types[symbol]
}

func (index *faultSourceIndex) underlyingType(typ faultSourceType) faultSourceType {
	seen := make(map[faultSourceSymbol]bool)
	for typ.expr != nil {
		typ.expr = ast.Unparen(typ.expr)
		switch expr := typ.expr.(type) {
		case *ast.StarExpr:
			typ.expr = expr.X
		case *ast.IndexExpr:
			typ.expr = expr.X
		case *ast.IndexListExpr:
			typ.expr = expr.X
		case *ast.Ident, *ast.SelectorExpr:
			symbol := index.typeSymbol(typ.source, typ.expr)
			decl := index.typeDeclaration(typ.source, symbol)
			if decl.typ == nil || seen[symbol] {
				return faultSourceType{}
			}
			seen[symbol] = true
			typ = faultSourceType{source: decl.source, expr: decl.typ.Type}
		default:
			return typ
		}
	}
	return faultSourceType{}
}

// Resolve fields and methods at the shallowest embedding depth. An own member
// shadows promotions, and competing members at the same depth are ambiguous.
func (index *faultSourceIndex) member(typ faultSourceType, name string) (faultSourceType, faultSourceSymbol) {
	seen := make(map[faultSourceType]int)
	for depth, level := 0, []faultSourceType{typ}; len(level) > 0; depth++ {
		var next []faultSourceType
		var fieldType faultSourceType
		var method faultSourceSymbol
		matches := 0
		for _, receiver := range level {
			if receiver.expr == nil {
				continue
			}
			if previous, ok := seen[receiver]; ok && previous < depth {
				continue
			}
			seen[receiver] = depth
			symbol := index.typeSymbol(receiver.source, receiver.expr)
			candidate := faultSourceSymbol{pkg: symbol.pkg, receiver: symbol.name, name: name, local: symbol.local}
			_, declared := index.functions[candidate]
			if declared || symbol == (faultSourceSymbol{pkg: faultImportPath, name: "Fault"}) && (name == "Is" || name == "PausePoint" || name == "WaitIf") {
				matches++
				method = candidate
				continue
			}
			underlying := index.underlyingType(receiver)
			structure, ok := underlying.expr.(*ast.StructType)
			if !ok {
				continue
			}
			for _, field := range structure.Fields.List {
				fieldNames := field.Names
				if len(fieldNames) == 0 {
					embedded := ast.Unparen(field.Type)
				nominal:
					for {
						switch typ := embedded.(type) {
						case *ast.StarExpr:
							embedded = ast.Unparen(typ.X)
						case *ast.IndexExpr:
							embedded = ast.Unparen(typ.X)
						case *ast.IndexListExpr:
							embedded = ast.Unparen(typ.X)
						default:
							break nominal
						}
					}
					if selector, ok := embedded.(*ast.SelectorExpr); ok {
						fieldNames = []*ast.Ident{selector.Sel}
					} else if identifier, ok := embedded.(*ast.Ident); ok {
						fieldNames = []*ast.Ident{identifier}
					}
					next = append(next, faultSourceType{source: underlying.source, expr: field.Type})
				}
				for _, fieldName := range fieldNames {
					if fieldName.Name == name {
						matches++
						fieldType = faultSourceType{source: underlying.source, expr: field.Type}
						method = candidate
					}
				}
			}
		}
		if matches > 1 {
			return faultSourceType{}, faultSourceSymbol{}
		}
		if matches == 1 {
			return fieldType, method
		}
		level = next
	}
	return faultSourceType{}, faultSourceSymbol{}
}

// This syntactic guard resolves direct and parenthesised receivers; indexed
// slices, arrays, maps and named collections; ranges over slices, arrays, maps
// and channels; channel receives; value, pointer, explicit, nested and generic
// embedding; local and package types; cross-file globals; alias and promoted
// method expressions, including instantiated concrete embeddings; instantiated
// generics with concrete results; visibly typed function values; function-call
// tuple results; and interface declarations with a visible fault initializer.
// Comma-ok declarations are not resolved as positional result bindings.
// Values whose fault origin is only known at run time are outside the guard:
// abstract interfaces, type parameters without a concrete embedding, and
// flow-sensitive reassignment must be caught by review.
func (index *faultSourceIndex) expressionType(source *faultSourceFile, expr ast.Expr) faultSourceType {
	seen := make(map[ast.Expr]bool)
	var resolve func(*faultSourceFile, ast.Expr) faultSourceType
	resultType := func(source *faultSourceFile, call *ast.CallExpr, position int) faultSourceType {
		symbol := index.callSymbol(source, call.Fun)
		if position == 0 && symbol.pkg == faultImportPath && symbol.receiver == "" && (symbol.name == "Active" || symbol.name == "None" || symbol.name == "FromList") {
			// Fixtures need not provide the imported fault package's sources.
			return faultSourceType{source: &faultSourceFile{pkg: faultImportPath}, expr: ast.NewIdent("Fault")}
		}
		var function *ast.FuncType
		if decl, ok := index.functions[symbol]; ok {
			source, function = decl.source, decl.fn.Type
		} else {
			callable := index.underlyingType(resolve(source, call.Fun))
			source = callable.source
			function, _ = callable.expr.(*ast.FuncType)
		}
		if function != nil && function.Results != nil {
			for _, field := range function.Results.List {
				count := max(1, len(field.Names))
				if position < count {
					return faultSourceType{source: source, expr: field.Type}
				}
				position -= count
			}
		}
		return faultSourceType{}
	}
	resolve = func(source *faultSourceFile, expr ast.Expr) faultSourceType {
		if expr == nil || seen[expr] {
			return faultSourceType{}
		}
		seen[expr] = true
		switch expr := ast.Unparen(expr).(type) {
		case *ast.Ident:
			var value *ast.ValueSpec
			if expr.Obj != nil {
				switch decl := expr.Obj.Decl.(type) {
				case *ast.Field:
					return faultSourceType{source: source, expr: decl.Type}
				case *ast.ValueSpec:
					value = decl
				case *ast.AssignStmt:
					for i, lhs := range decl.Lhs {
						if name, ok := lhs.(*ast.Ident); !ok || name.Obj != expr.Obj {
							continue
						}
						if len(decl.Rhs) == 1 {
							if ranged, ok := decl.Rhs[0].(*ast.UnaryExpr); ok && ranged.Op == token.RANGE {
								collection := index.underlyingType(resolve(source, ranged.X))
								switch typ := collection.expr.(type) {
								case *ast.ArrayType:
									if i == 1 {
										return faultSourceType{source: collection.source, expr: typ.Elt}
									}
								case *ast.ChanType:
									return faultSourceType{source: collection.source, expr: typ.Value}
								case *ast.MapType:
									if i == 0 {
										return faultSourceType{source: collection.source, expr: typ.Key}
									}
									return faultSourceType{source: collection.source, expr: typ.Value}
								}
								return faultSourceType{}
							}
							if call, ok := ast.Unparen(decl.Rhs[0]).(*ast.CallExpr); ok && len(decl.Lhs) > 1 {
								return resultType(source, call, i)
							}
						}
						if i < len(decl.Rhs) {
							return resolve(source, decl.Rhs[i])
						}
					}
				case *ast.TypeSpec:
					return faultSourceType{source: source, expr: expr}
				}
			} else if decl, ok := index.variables[faultSourceSymbol{pkg: source.pkg, name: expr.Name}]; ok {
				source, value = decl.source, decl.value
			}
			if value != nil {
				var declared faultSourceType
				if value.Type != nil {
					declared = faultSourceType{source: source, expr: value.Type}
					if _, isInterface := index.underlyingType(declared).expr.(*ast.InterfaceType); !isInterface {
						return declared
					}
				}
				for i, name := range value.Names {
					if name.Name != expr.Name {
						continue
					}
					var initialized faultSourceType
					if len(value.Values) == 1 && len(value.Names) > 1 {
						if call, ok := ast.Unparen(value.Values[0]).(*ast.CallExpr); ok {
							initialized = resultType(source, call, i)
						}
					} else if i < len(value.Values) {
						initialized = resolve(source, value.Values[i])
					}
					if declared.expr == nil {
						return initialized
					}
					// Retain a directly visible fault initializer of an interface.
					if _, method := index.member(initialized, "Is"); method == (faultSourceSymbol{pkg: faultImportPath, receiver: "Fault", name: "Is"}) {
						return initialized
					}
					break
				}
				return declared
			}
		case *ast.UnaryExpr:
			if expr.Op == token.ARROW {
				channel := index.underlyingType(resolve(source, expr.X))
				if typ, ok := channel.expr.(*ast.ChanType); ok {
					return faultSourceType{source: channel.source, expr: typ.Value}
				}
				return faultSourceType{}
			}
			return resolve(source, expr.X)
		case *ast.StarExpr:
			return resolve(source, expr.X)
		case *ast.TypeAssertExpr:
			return faultSourceType{source: source, expr: expr.Type}
		case *ast.CompositeLit:
			return faultSourceType{source: source, expr: expr.Type}
		case *ast.IndexExpr, *ast.IndexListExpr:
			if index.typeDeclaration(source, index.typeSymbol(source, expr)).typ != nil {
				return faultSourceType{source: source, expr: expr}
			}
			if indexed, ok := expr.(*ast.IndexExpr); ok {
				collection := index.underlyingType(resolve(source, indexed.X))
				switch typ := collection.expr.(type) {
				case *ast.ArrayType:
					return faultSourceType{source: collection.source, expr: typ.Elt}
				case *ast.MapType:
					return faultSourceType{source: collection.source, expr: typ.Value}
				}
			}
		case *ast.FuncLit:
			return faultSourceType{source: source, expr: expr.Type}
		case *ast.CallExpr:
			if result := resultType(source, expr, 0); result.expr != nil {
				return result
			}
			return faultSourceType{source: source, expr: expr.Fun}
		case *ast.SelectorExpr:
			if pkg, ok := expr.X.(*ast.Ident); ok && pkg.Obj == nil && source.imports[pkg.Name] != "" {
				return faultSourceType{source: source, expr: expr}
			}
			typ, _ := index.member(resolve(source, expr.X), expr.Sel.Name)
			return typ
		}
		return faultSourceType{}
	}
	return resolve(source, expr)
}

func (index *faultSourceIndex) callSymbol(source *faultSourceFile, expr ast.Expr) faultSourceSymbol {
	seen := make(map[ast.Expr]bool)
	var resolve func(ast.Expr) faultSourceSymbol
	resolve = func(expr ast.Expr) faultSourceSymbol {
		if seen[expr] {
			return faultSourceSymbol{}
		}
		seen[expr] = true
		switch expr := ast.Unparen(expr).(type) {
		case *ast.Ident:
			if expr.Obj != nil {
				switch decl := expr.Obj.Decl.(type) {
				case *ast.AssignStmt:
					for i, lhs := range decl.Lhs {
						if name, ok := lhs.(*ast.Ident); ok && name.Obj == expr.Obj && i < len(decl.Rhs) {
							return resolve(decl.Rhs[i])
						}
					}
				case *ast.ValueSpec:
					for i, name := range decl.Names {
						if name.Obj == expr.Obj && i < len(decl.Values) {
							return resolve(decl.Values[i])
						}
					}
				case *ast.FuncDecl:
					return source.functionSymbol(decl)
				}
				return faultSourceSymbol{}
			}
			return faultSourceSymbol{pkg: source.pkg, name: expr.Name}
		case *ast.IndexExpr:
			return resolve(expr.X)
		case *ast.IndexListExpr:
			return resolve(expr.X)
		case *ast.SelectorExpr:
			if pkg, ok := expr.X.(*ast.Ident); ok && pkg.Obj == nil && source.imports[pkg.Name] != "" {
				return faultSourceSymbol{pkg: source.imports[pkg.Name], name: expr.Sel.Name}
			}
			receiver := index.expressionType(source, expr.X)
			if _, method := index.member(receiver, expr.Sel.Name); method != (faultSourceSymbol{}) {
				return method
			}
			symbol := index.typeSymbol(receiver.source, receiver.expr)
			return faultSourceSymbol{pkg: symbol.pkg, receiver: symbol.name, name: expr.Sel.Name, local: symbol.local}
		}
		return faultSourceSymbol{}
	}
	return resolve(expr)
}
