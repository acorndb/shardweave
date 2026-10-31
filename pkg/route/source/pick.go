package source

import (
	"github.com/acorndb/prism/pkg/constant"
	"github.com/acorndb/prism/pkg/mysql"
	"github.com/acorndb/prism/pkg/session"
)

// Pick chooses master vs slave intent for one command (design §5.10).
// Session Layer A overrides statement Layer B.
func Pick(st *session.State, cmd byte, sql string) constant.Role {
	if st != nil && st.NeedsSticky() {
		return constant.RoleMaster
	}
	switch cmd {
	case mysql.ComStmtPrepare, mysql.ComStmtExecute, mysql.ComStmtReset,
		mysql.ComStmtClose, mysql.ComStmtSendLongData:
		return constant.RoleMaster // B11
	case mysql.ComInitDB:
		return constant.RoleSlave // M5 when not sticky
	case mysql.ComFieldList:
		return constant.RoleSlave
	case mysql.ComQuery:
		if ClassifyWantSlave(sql) {
			return constant.RoleSlave
		}
		return constant.RoleMaster
	default:
		return constant.RoleMaster
	}
}
