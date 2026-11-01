//go:build darwin || linux

package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
)

const instanceLockName = "workspace.lock"

type instanceLock struct{ file *os.File }

// This is coordination among cooperating local instances, not authentication or
// a sandbox. Keep this inode stable: never truncate or unlink the lock file.
func lockDataDir(dir string) (io.Closer, error) {
	// OpenFile sets close-on-exec. O_NONBLOCK also prevents a hostile FIFO from
	// blocking startup before fstat can reject it; O_NOFOLLOW rejects symlinks.
	file, err := os.OpenFile(filepath.Join(dir, instanceLockName), os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0600)
	if err != nil {
		return nil, fmt.Errorf("打开数据目录进程锁失败：%w", err)
	}
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		file.Close()
		return nil, errors.New("数据目录进程锁必须是普通文件")
	}
	if err := flock(file, syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		file.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, errors.New("数据目录已被其他本地实例使用")
		}
		return nil, fmt.Errorf("获取数据目录进程锁失败：%w", err)
	}
	return &instanceLock{file}, nil
}

func flock(file *os.File, operation int) error {
	for {
		err := syscall.Flock(int(file.Fd()), operation)
		if err != syscall.EINTR {
			return err
		}
	}
}

func (lock *instanceLock) Close() error {
	unlockErr := flock(lock.file, syscall.LOCK_UN)
	if err := errors.Join(unlockErr, lock.file.Close()); err != nil {
		return fmt.Errorf("释放数据目录进程锁失败：%w", err)
	}
	return nil
}
