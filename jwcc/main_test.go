package jwcc_test

import (
	"testing"

	"github.com/djylb/cfgfile/internal/leakcheck"
)

func TestMain(m *testing.M) { leakcheck.VerifyTestMain(m) }
