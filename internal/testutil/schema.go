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

package testutil

import (
	"bytes"
	"fmt"
	"testing"

	jsonschema "github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/zchee/agentctl/schemas"
)

// SchemaError validates document against the named embedded schema
// (draft 2020-12) and returns the validation error, or nil when the
// document conforms. name is the schema's file name, such as
// "status.v1.json". Schema validation proves shape, not byte order.
func SchemaError(name string, document []byte) error {
	schemaBytes, err := schemas.FS.ReadFile(name)
	if err != nil {
		return fmt.Errorf("read embedded schema %q: %w", name, err)
	}
	schemaDoc, err := jsonschema.UnmarshalJSON(bytes.NewReader(schemaBytes))
	if err != nil {
		return fmt.Errorf("parse schema %q: %w", name, err)
	}
	compiler := jsonschema.NewCompiler()
	compiler.DefaultDraft(jsonschema.Draft2020)
	if err := compiler.AddResource(name, schemaDoc); err != nil {
		return fmt.Errorf("add schema %q: %w", name, err)
	}
	schema, err := compiler.Compile(name)
	if err != nil {
		return fmt.Errorf("compile schema %q: %w", name, err)
	}
	instance, err := jsonschema.UnmarshalJSON(bytes.NewReader(document))
	if err != nil {
		return fmt.Errorf("parse the document: %w", err)
	}
	return schema.Validate(instance)
}

// ValidateSchema fails the test when document does not conform to the
// named embedded schema.
func ValidateSchema(tb testing.TB, name string, document []byte) {
	tb.Helper()
	if err := SchemaError(name, document); err != nil {
		tb.Fatalf("the document does not conform to %s:\n%v\ndocument:\n%s", name, err, document)
	}
}
