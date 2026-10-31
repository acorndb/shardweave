package execute

import (
	"context"

	"github.com/acorndb/shardweave/pkg/mysql"
	"github.com/acorndb/shardweave/pkg/mysql/packet"
)

// rawForwarder is implemented by backend streams that can forward protocol packets.
type rawForwarder interface {
	ForwardTo(ctx context.Context, front *packet.Conn) (mysql.ResponseInfo, error)
}

// prepareForwarder rewrites the backend statement id for COM_STMT_PREPARE.
type prepareForwarder interface {
	ForwardPrepareTo(front *packet.Conn, clientID uint32) (mysql.ResponseInfo, uint32, error)
}
