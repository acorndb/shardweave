package backend

import (
	"context"
	"io"

	"github.com/acorndb/prism/pkg/mysql"
	"github.com/acorndb/prism/pkg/mysql/packet"
	"github.com/acorndb/prism/pkg/result"
)

// rawStream forwards the backend response packets to the frontend unchanged.
// It implements result.Stream (logical methods are unused for N=1 passthrough)
// and the rawForwarder interface consumed by pkg/execute.
type rawStream struct {
	pc      *packet.Conn
	prepare bool
}

func (s *rawStream) ForwardTo(ctx context.Context, front *packet.Conn) (uint16, bool, error) {
	_ = ctx
	if s.prepare {
		return 0, false, mysql.ForwardPrepareResponse(front, s.pc, true)
	}
	return mysql.ForwardResponseResult(front, s.pc, true)
}

func (s *rawStream) Fields() []result.Field { return nil }

func (s *rawStream) Next() (result.Row, error) { return nil, io.EOF }

func (s *rawStream) Close() error { return nil }

// noReplyStream is used for commands that expect no response (COM_STMT_CLOSE).
type noReplyStream struct{}

func (noReplyStream) ForwardTo(context.Context, *packet.Conn) (uint16, bool, error) {
	return 0, false, nil
}

func (noReplyStream) Fields() []result.Field { return nil }

func (noReplyStream) Next() (result.Row, error) { return nil, io.EOF }

func (noReplyStream) Close() error { return nil }
