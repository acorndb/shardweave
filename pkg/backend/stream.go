package backend

import (
	"context"
	"io"

	"github.com/acorndb/shardweave/pkg/mysql"
	"github.com/acorndb/shardweave/pkg/mysql/packet"
	"github.com/acorndb/shardweave/pkg/result"
)

// rawStream forwards the backend response packets to the frontend unchanged.
// It implements result.Stream (logical methods are unused for N=1 passthrough)
// and the rawForwarder interface consumed by pkg/execute.
type rawStream struct {
	pc      *packet.Conn
	prepare bool
}

func (s *rawStream) ForwardTo(ctx context.Context, front *packet.Conn) (mysql.ResponseInfo, error) {
	_ = ctx
	if s.prepare {
		return mysql.ResponseInfo{}, mysql.ForwardPrepareResponse(front, s.pc, true)
	}
	return mysql.ForwardResponseInfo(front, s.pc, true)
}

func (s *rawStream) Fields() []result.Field { return nil }

func (s *rawStream) Next() (result.Row, error) { return nil, io.EOF }

func (s *rawStream) Close() error { return nil }

// noReplyStream is used for commands that expect no response (COM_STMT_CLOSE).
type noReplyStream struct{}

func (noReplyStream) ForwardTo(context.Context, *packet.Conn) (mysql.ResponseInfo, error) {
	return mysql.ResponseInfo{}, nil
}

func (noReplyStream) Fields() []result.Field { return nil }

func (noReplyStream) Next() (result.Row, error) { return nil, io.EOF }

func (noReplyStream) Close() error { return nil }
