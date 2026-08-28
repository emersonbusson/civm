//go:build !linux

package jitdispatcher

import "os"

func ownedByCurrentUser(os.FileInfo) bool {
	return false
}

func ownedByTrustedAdmin(os.FileInfo) bool { return false }
