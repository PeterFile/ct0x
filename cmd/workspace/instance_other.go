//go:build !darwin && !linux

package main

import (
	"errors"
	"io"
)

func lockDataDir(string) (io.Closer, error) {
	return nil, errors.New("当前平台不支持数据目录进程锁，仅支持 Darwin/Linux")
}
