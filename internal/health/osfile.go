package health

import "os"

// osReadFile is a thin alias to os.ReadFile so tests can swap if needed.
var osReadFile = os.ReadFile
