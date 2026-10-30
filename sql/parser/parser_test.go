package parser_test

import (
	"testing"

	"github.com/sharding-proxy/sql/parser"
	"github.com/stretchr/testify/require"
)

func TestParse_Select(t *testing.T) {
	res, err := parser.Parse("SELECT 1")
	require.NoError(t, err)
	require.Len(t, res.Stmts, 1)
}

func TestParse_SyntaxError(t *testing.T) {
	_, err := parser.Parse("SELEC T")
	require.Error(t, err)
}

func TestParse_Empty(t *testing.T) {
	res, err := parser.Parse("")
	require.NoError(t, err)
	require.Empty(t, res.Stmts)
}

func TestParse_MultiStatements(t *testing.T) {
	res, err := parser.Parse("SELECT 1; SELECT 2")
	require.NoError(t, err)
	require.Len(t, res.Stmts, 2)
}
