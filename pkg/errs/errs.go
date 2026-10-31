// Package errs defines the MySQL error value shared by the protocol, planning
// and execution layers, plus sentinel errors that cross package boundaries.
// Package-local parsing/diagnostic errors stay close to their source; this
// package is for values that must be recognized (errors.Is/As) or written to
// clients consistently.
package errs

import (
	"errors"
	"fmt"
)

// MySQL error numbers used by the proxy (MySQL ER_* names).
const (
	ErTooManyConnections = 1040
	ErBadHandshake       = 1043
	ErAccessDenied       = 1045
	ErUnknownComError    = 1047
	ErUnknownError       = 1105
	ErPacketTooLarge     = 1153
	ErNotSupportedYet    = 1235
)

// SQLSTATE values used with the error numbers above.
const (
	StateGeneral      = "HY000"
	StateSyntax       = "42000"
	StateConnection   = "08S01"
	StateTooManyConns = "08004"
	StateAccessDenied = "28000"
)

var (
	// ErrNoResultSet means the backend returned an OK/DDL response, not a
	// result set.
	ErrNoResultSet = errors.New("backend: response is not a result set")
	// ErrPoolExhausted means the pool is at capacity and acquire_timeout is 0,
	// or the acquire wait expired.
	ErrPoolExhausted = errors.New("pool: exhausted")
	// ErrPacketTooLarge means a logical packet exceeds max_allowed_packet.
	ErrPacketTooLarge = errors.New("packet: payload exceeds max_allowed_packet")
)

// MySQL is one MySQL ERR packet: error number, SQLSTATE and message. It is
// used both for errors synthesized by the proxy and for errors decoded from a
// backend, so callers can match a single type with errors.As.
type MySQL struct {
	Number   uint16
	SQLState string
	Message  string
}

func (e *MySQL) Error() string {
	if e == nil {
		return ""
	}
	state := e.SQLState
	if state == "" {
		state = StateGeneral
	}
	return fmt.Sprintf("Error %d (%s): %s", e.Number, state, e.Message)
}

// New builds a MySQL error with an explicit SQLSTATE.
func New(number uint16, state, message string) *MySQL {
	return &MySQL{Number: number, SQLState: state, Message: message}
}

// Errorf builds a MySQL error with a formatted message.
func Errorf(number uint16, state, format string, args ...any) *MySQL {
	return New(number, state, fmt.Sprintf(format, args...))
}

// Unsupported is ER_NOT_SUPPORTED_YET (1235/42000).
func Unsupported(format string, args ...any) *MySQL {
	return Errorf(ErNotSupportedYet, StateSyntax, format, args...)
}

// Unknown is ER_UNKNOWN_ERROR (1105/HY000).
func Unknown(format string, args ...any) *MySQL {
	return Errorf(ErUnknownError, StateGeneral, format, args...)
}

// UnknownCommand is ER_UNKNOWN_COM_ERROR (1047/08S01).
func UnknownCommand(cmd byte) *MySQL {
	return Errorf(ErUnknownComError, StateConnection, "command %d not supported yet", cmd)
}

// TooManyConnections is ER_CON_COUNT_ERROR (1040/08004).
func TooManyConnections() *MySQL {
	return New(ErTooManyConnections, StateTooManyConns, "Too many connections")
}

// PacketTooLarge is ER_NET_PACKET_TOO_LARGE (1153/08S01).
func PacketTooLarge() *MySQL {
	return New(ErPacketTooLarge, StateConnection, "Got a packet bigger than 'max_allowed_packet' bytes")
}

// AccessDenied is ER_ACCESS_DENIED_ERROR (1045/28000).
func AccessDenied(user string) *MySQL {
	return Errorf(ErAccessDenied, StateAccessDenied, "Access denied for user '%s'", user)
}

// BadHandshake is ER_HANDSHAKE_ERROR (1043/08S01).
func BadHandshake(cause error) *MySQL {
	if cause == nil {
		return New(ErBadHandshake, StateConnection, "Bad handshake")
	}
	return New(ErBadHandshake, StateConnection, "Bad handshake: "+cause.Error())
}
