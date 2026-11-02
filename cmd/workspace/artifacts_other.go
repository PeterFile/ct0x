//go:build !unix

package main

import (
	"errors"
	"io"

	"agentworkspace/internal/httpapi"
	"agentworkspace/internal/store"
)

func artifactReadRoutes(dataDir string, db *store.Store) ([]httpapi.Route, io.Closer, error) {
	return nil, nil, errors.New("当前平台不支持成果读取；本地进程仍仅支持 Darwin/Linux")
}
