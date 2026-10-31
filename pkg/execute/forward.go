package execute

import (
	"context"

	"github.com/acorndb/prism/pkg/mysql/packet"
)

// rawForwarder is implemented by backend streams that can forward protocol packets.
type rawForwarder interface {
	ForwardTo(ctx context.Context, front *packet.Conn) (status uint16, serverErr bool, err error)
}
