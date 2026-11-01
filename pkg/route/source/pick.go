package source

import (
	"github.com/acorndb/shardweave/pkg/constant"
	"github.com/acorndb/shardweave/pkg/mysql"
	"github.com/acorndb/shardweave/pkg/session"
	"github.com/acorndb/shardweave/pkg/sql"
)

// PickRole chooses master vs slave intent for one command (design §5.10).
// Session Layer A overrides statement Layer B; Layer B reads the statement's
// single Class snapshot instead of re-scanning raw SQL.
func PickRole(st *session.State, stmt *sql.Statement) constant.Role {
	if st != nil && st.NeedsSticky() {
		return constant.RoleMaster
	}
	if stmt == nil {
		return constant.RoleMaster
	}
	switch stmt.Cmd {
	case mysql.ComStmtPrepare, mysql.ComStmtExecute, mysql.ComStmtReset,
		mysql.ComStmtClose, mysql.ComStmtSendLongData:
		return constant.RoleMaster // B11
	case mysql.ComInitDB:
		return constant.RoleSlave // M5 when not sticky
	case mysql.ComFieldList:
		return constant.RoleSlave
	case mysql.ComQuery:
		if stmt.EnsureClass().WantSlave() {
			return constant.RoleSlave
		}
		return constant.RoleMaster
	default:
		return constant.RoleMaster
	}
}
