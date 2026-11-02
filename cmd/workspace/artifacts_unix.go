//go:build unix

package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"syscall"

	"agentworkspace/internal/artifactfs"
	"agentworkspace/internal/httpapi"
	"agentworkspace/internal/platform"
	"agentworkspace/internal/store"
)

const artifactReadLimit = 64 << 20

func artifactReadRoutes(dataDir string, db *store.Store) ([]httpapi.Route, io.Closer, error) {
	return artifactReadRoutesWithSync(dataDir, db, (*os.File).Sync)
}

func artifactReadRoutesWithSync(dataDir string, db *store.Store, syncFile func(*os.File) error) (routes []httpapi.Route, closer io.Closer, err error) {
	path, err := privateExecutionRoot(dataDir, "artifacts")
	if err != nil {
		return nil, nil, fmt.Errorf("准备成果私有根失败：%w", err)
	}
	root, err := os.OpenRoot(path)
	if err != nil {
		return nil, nil, fmt.Errorf("打开成果私有根失败：%w", err)
	}
	defer func() {
		err = errors.Join(err, root.Close())
		if err != nil {
			if closer != nil {
				err = errors.Join(err, closer.Close())
			}
			routes, closer = nil, nil
			err = fmt.Errorf("构造成果读取入口失败：%w", err)
		}
	}()
	if err := artifactReadPolicy(root, syncFile); err != nil {
		return nil, nil, err
	}
	// Retry on reopen too: a failed parent sync leaves the root in place.
	parent, err := os.Open(dataDir)
	if err != nil {
		return nil, nil, err
	}
	if err := errors.Join(syncFile(parent), parent.Close()); err != nil {
		return nil, nil, fmt.Errorf("同步成果根父目录失败：%w", err)
	}
	blobs, err := artifactfs.New(root, artifactReadLimit)
	if err != nil {
		return nil, nil, err
	}
	closer = blobs
	service, err := platform.NewArtifacts(db, blobs)
	if err != nil {
		return nil, closer, err
	}
	return httpapi.ArtifactRoutes(service), closer, nil
}

func artifactReadPolicy(root *os.Root, syncFile func(*os.File) error) (err error) {
	blobs, err := root.Lstat("blobs")
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if blobs != nil && blobs.Mode() != os.ModeDir|0700 {
		return errors.New("成果 blobs 必须是非符号链接的 0700 私有目录，未更改原条目")
	}
	dir, err := root.Open(".")
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, dir.Close()) }()
	marker, err := root.Lstat("read-limit")
	recorded := err == nil
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if recorded {
		if marker.Mode() != 0600 {
			return errors.New("成果读取上限标记必须是非符号链接的 0600 普通文件，未覆盖原标记")
		}
		file, openErr := root.OpenFile("read-limit", os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
		if openErr != nil {
			return openErr
		}
		defer func() { err = errors.Join(err, file.Close()) }()
		data, readErr := io.ReadAll(io.LimitReader(file, 32))
		if readErr != nil {
			return readErr
		}
		limit, parseErr := strconv.ParseUint(strings.TrimSuffix(string(data), "\n"), 10, 64)
		if parseErr != nil || string(data) != strconv.FormatUint(limit, 10)+"\n" {
			return errors.New("成果读取上限标记无效或初始化未完成，未覆盖原标记")
		}
		if limit != artifactReadLimit {
			return fmt.Errorf("成果读取上限不兼容：已记录 %d，应用要求 %d；未更改成果或标记", limit, artifactReadLimit)
		}
		if err := syncFile(file); err != nil {
			return fmt.Errorf("同步成果读取上限标记失败，保留原标记：%w", err)
		}
	}
	// Only two names are supported; three entries suffice to detect unknown layouts.
	entries, readErr := dir.ReadDir(3)
	if readErr != nil && readErr != io.EOF {
		return readErr
	}
	for _, entry := range entries {
		if entry.Name() != "blobs" && entry.Name() != "read-limit" {
			return errors.New("成果库布局不明，未更改成果；需显式采用策略")
		}
	}
	if blobs != nil {
		blobDir, openErr := root.Open("blobs")
		if openErr != nil {
			return openErr
		}
		defer func() { err = errors.Join(err, blobDir.Close()) }()
		for {
			entries, readErr := blobDir.ReadDir(64)
			for _, entry := range entries {
				if !recorded {
					return errors.New("现有成果库尚未记录可信读取上限，未更改成果；需显式采用策略")
				}
				if len(entry.Name()) != 64 || strings.Trim(entry.Name(), "0123456789abcdef") != "" {
					continue
				}
				info, err := entry.Info()
				if err != nil {
					return err
				}
				if info.Mode().IsRegular() && info.Size() > artifactReadLimit {
					return fmt.Errorf("成果大小与读取上限不兼容：%s 为 %d 字节，上限 %d；未更改成果", entry.Name(), info.Size(), artifactReadLimit)
				}
			}
			if readErr == io.EOF {
				break
			}
			if readErr != nil {
				return readErr
			}
		}
	}
	if recorded {
		return nil
	}
	file, err := root.OpenFile("read-limit", os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return fmt.Errorf("初始化成果读取上限失败，未覆盖已有标记：%w", err)
	}
	_, writeErr := file.WriteString("67108864\n")
	if err := errors.Join(writeErr, file.Chmod(0600), syncFile(file), file.Close(), syncFile(dir)); err != nil {
		return fmt.Errorf("成果读取上限初始化未完成，保留标记供诊断：%w", err)
	}
	return nil
}
