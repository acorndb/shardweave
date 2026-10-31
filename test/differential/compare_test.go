package differential

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCompareStep_SkipsWarningsWhenUnsupported(t *testing.T) {
	steps := []Step{{Name: "warn"}}
	direct := []StepResult{{
		Warnings: []WarningRow{{Level: "Warning", Code: "1265", Message: "data truncated"}},
	}}

	t.Run("proxy_rejects_show_warnings", func(t *testing.T) {
		proxied := []StepResult{{WarningsUnsupported: true}}
		require.Empty(t, Compare(steps, direct, proxied),
			"SHOW WARNINGS 被显式拒绝时，warning 差异不应计入")
	})

	t.Run("both_support_show_warnings", func(t *testing.T) {
		proxied := []StepResult{{}}
		require.NotEmpty(t, Compare(steps, direct, proxied),
			"两端都支持 SHOW WARNINGS 时仍应比较 warning")
	})
}
