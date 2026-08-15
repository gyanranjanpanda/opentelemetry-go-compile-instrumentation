// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package genai

import (
	"go/parser"
	"go/token"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestAdapterFileImportsNothing enforces the central claim of the design: a
// provider adapter is written against adapter.go alone, and adapter.go depends
// on nothing. If this test starts failing because a tracing, attribute or
// metric type crept into the contract, the adapter interface has stopped being
// provider-facing and the leak needs undoing, not allowing.
func TestAdapterFileImportsNothing(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "adapter.go", nil, parser.ImportsOnly)
	require.NoError(t, err)

	imports := make([]string, 0, len(file.Imports))
	for _, spec := range file.Imports {
		path, unquoteErr := strconv.Unquote(spec.Path.Value)
		require.NoError(t, unquoteErr)
		imports = append(imports, path)
	}

	assert.Empty(t, imports,
		"adapter.go must import nothing at all, so no adapter can inherit an OpenTelemetry dependency from it")
}

// TestAdapterInterfaceIsSatisfiableWithoutOTel is the compile-time half of the
// same claim: this adapter is defined with no OpenTelemetry import in scope.
func TestAdapterInterfaceIsSatisfiableWithoutOTel(t *testing.T) {
	var a Adapter = stubAdapter{provider: "stub"}
	assert.Equal(t, ProviderID("stub"), a.Provider())

	var sa StreamAdapter = stubStreamAdapter{stubAdapter: stubAdapter{provider: "stub"}}
	assert.Equal(t, ProviderID("stub"), sa.Provider())
}

func TestOperationUnknownIsTheZeroValue(t *testing.T) {
	var op Operation
	assert.Equal(t, OperationUnknown, op)
	assert.Empty(t, string(OperationUnknown))
}

func TestPointerHelpers(t *testing.T) {
	assert.Equal(t, int64(7), *Int64(7))
	assert.InDelta(t, 0.5, *Float64(0.5), 0.0001)
}
