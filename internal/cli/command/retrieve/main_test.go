package retrieve

import (
	"os"
	"testing"

	"github.com/entireio/entire-brain/internal/cli/internal/clitest"
)

func TestMain(m *testing.M) { os.Exit(clitest.PinXDGRoots(m)) }
