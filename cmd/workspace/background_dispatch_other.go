//go:build !darwin && !linux

package main

import (
	"errors"
	"os"
)

func openBackgroundDispatchFile(string) (*os.File, error) {
	return nil, errors.New("群组后台调度预览仅支持 Darwin/Linux")
}

func backgroundFileOwned(os.FileInfo) bool {
	return false
}
