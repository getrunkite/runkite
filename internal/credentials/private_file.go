package credentials

import (
	"io"
	"os"
)

// ReadPrivateFile rejects links and group/world permissions. It rechecks the
// opened inode so replacing the path between Lstat and Open fails closed.
func ReadPrivateFile(path string, limit int64) ([]byte, error) {
	if path == "" || limit <= 0 {
		return nil, ErrUnavailable
	}
	before, err := os.Lstat(path)
	if err != nil || !before.Mode().IsRegular() || before.Mode().Perm()&0077 != 0 || !ownedByProcess(before) {
		return nil, ErrUnavailable
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, ErrUnavailable
	}
	defer f.Close()
	after, err := f.Stat()
	if err != nil || !os.SameFile(before, after) || !after.Mode().IsRegular() || after.Mode().Perm()&0077 != 0 || !ownedByProcess(after) {
		return nil, ErrUnavailable
	}
	b, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil || len(b) == 0 || int64(len(b)) > limit {
		return nil, ErrUnavailable
	}
	return b, nil
}
