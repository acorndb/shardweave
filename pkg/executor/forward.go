package executor

import (
	"context"
	"fmt"

	"github.com/acorndb/prism/pkg/backend"
	"github.com/acorndb/prism/pkg/mysql"
	"github.com/acorndb/prism/pkg/mysql/packet"
	"github.com/acorndb/prism/pkg/session"
)

func alignBackend(ctx context.Context, b backend.Conn, st *session.State) error {
	a, ok := b.(interface {
		Align(context.Context, string, string) error
	})
	if !ok {
		return nil
	}
	return a.Align(ctx, st.Database(), st.Charset())
}

func writeBackend(b backend.Conn, cmd []byte) error {
	mb, ok := b.(*backend.MySQLConn)
	if !ok {
		return fmt.Errorf("executor: backend type %T not supported", b)
	}
	back := mb.Packet()
	back.ResetSequence()
	buf := make([]byte, 4+len(cmd))
	copy(buf[4:], cmd)
	return back.WritePacket(buf)
}

func execCommand(front *packet.Conn, b backend.Conn, st *session.State, cmd []byte, prepare bool) error {
	mb, ok := b.(*backend.MySQLConn)
	if !ok {
		return fmt.Errorf("executor: backend type %T not supported", b)
	}
	back := mb.Packet()
	back.ResetSequence()
	buf := make([]byte, 4+len(cmd))
	copy(buf[4:], cmd)
	if err := back.WritePacket(buf); err != nil {
		return err
	}
	const deprecateEOF = true
	if prepare {
		return mysql.ForwardPrepareResponse(front, back, deprecateEOF)
	}
	status, err := mysql.ForwardResponse(front, back, deprecateEOF)
	if err != nil {
		return err
	}
	st.SetInTx(status&mysql.ServerStatusInTrans != 0)
	return nil
}
