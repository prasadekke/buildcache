//go:build !unix

package sockpath

import "os"

func ownerUID(os.FileInfo) (int, bool) { return 0, false }
