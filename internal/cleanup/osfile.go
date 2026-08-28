package cleanup

import (
	"io/fs"
	"os"
)

func defaultStatImpl(path string) (fs.FileInfo, error) {
	return os.Stat(path)
}
