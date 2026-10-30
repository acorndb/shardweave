package router

import (
	"github.com/acorndb/prism/pkg/mysql"
	"github.com/acorndb/prism/pkg/session"
)

// Pick chooses master vs slave intent for one command (design §5.10).
// Session Layer A overrides statement Layer B.
func Pick(st *session.State, cmd byte, sql string) TargetRole {
	if st != nil && st.NeedsSticky() {
		return RoleMaster
	}
	switch cmd {
	case mysql.ComStmtPrepare, mysql.ComStmtExecute, mysql.ComStmtReset,
		mysql.ComStmtClose, mysql.ComStmtSendLongData:
		return RoleMaster // B11
	case mysql.ComInitDB:
		return RoleSlave // M5 when not sticky
	case mysql.ComFieldList:
		return RoleSlave
	case mysql.ComQuery:
		if ClassifyWantSlave(sql) {
			return RoleSlave
		}
		return RoleMaster
	default:
		return RoleMaster
	}
}
