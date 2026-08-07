//go:build !slatedb

package storagetest

import (
	"fmt"
	"testing"

	"github.com/Zamua/hostthis/internal/storage"
)

// config keeps the store in memory: nothing to install, nothing to clean up.
func config(t *testing.T) storage.ShaleConfig {
	t.Helper()
	return storage.ShaleConfig{DbName: fmt.Sprintf("test-%d", seq.Load()+1)}
}
