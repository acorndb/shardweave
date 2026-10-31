package result_test

import (
	"io"
	"testing"

	"github.com/acorndb/prism/pkg/result"
	"github.com/stretchr/testify/require"
)

type fakeStream struct {
	closed bool
}

func (s *fakeStream) Fields() []result.Field { return []result.Field{{Name: "id"}} }
func (s *fakeStream) Next() (result.Row, error) {
	return nil, io.EOF
}

func (s *fakeStream) Close() error {
	s.closed = true
	return nil
}

func TestStreamContract(t *testing.T) {
	var s result.Stream = &fakeStream{}
	require.Len(t, s.Fields(), 1)
	_, err := s.Next()
	require.ErrorIs(t, err, io.EOF)
	require.NoError(t, s.Close())
	require.True(t, s.(*fakeStream).closed)
}
