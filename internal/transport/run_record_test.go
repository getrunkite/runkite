package transport_test

import (
	"testing"

	"github.com/getrunkite/runkite/internal/transport"
)

func TestRecordNamespacesPreserveExactTupleBoundaries(t *testing.T) {
	seen := map[string]bool{}
	for _, tuple := range [][2]string{{"same", ""}, {"same", "old"}, {"same", "new"}, {"a\x00b", "c"}, {"a", "b\x00c"}, {"\xff", "same"}, {"\xfe", "same"}, {"same", "\x00"}} {
		key := transport.RunRecordKey(tuple[0], tuple[1])
		if seen[key] || key != transport.RunRecordKey(tuple[0], tuple[1]) {
			t.Fatal("record namespace aliases or changes")
		}
		seen[key] = true
	}
}
