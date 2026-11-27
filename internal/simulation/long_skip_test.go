package mockup

import (
	"os"
	"testing"

	"github.com/indexus/go-indexus-core/internal/testkit"
)

func skipLong(t testing.TB) {
	t.Helper()
	testkit.RequireLong(t)
}

func TestMain(m *testing.M) {
	if !testkit.Long() {
		os.Exit(0)
	}
	os.Exit(m.Run())
}
