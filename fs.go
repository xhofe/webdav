package webdav

import (
	"context"
	"net/http"
	"os"
)

type GetReq struct {
	Path        string
	WithContent bool
}

type ListReq struct {
	DirPath string
}

type MkdirReq struct {
	DirPath string
	Mode    os.FileMode
}

type RemoveReq struct {
	Path string
}

type RenameReq struct {
	OldPath string
	NewPath string
}

type CopyMoveReq struct {
	SrcPath   string
	DstPath   string
	Overwrite bool
	Depth     int
}

type PutReq struct {
	Path string
	File Obj
}

type ServeFileReq struct {
	Req        *http.Request
	RespWriter http.ResponseWriter
	File       Obj
}

type FS interface {
	Get(ctx context.Context, req GetReq) (Obj, error)
	List(ctx context.Context, req ListReq) ([]ObjInfo, error)
	MkdirAll(ctx context.Context, req MkdirReq) error
	RemoveAll(ctx context.Context, req RemoveReq) error
	Rename(ctx context.Context, req RenameReq) error
	CopyAll(ctx context.Context, req CopyMoveReq) error
	MoveAll(ctx context.Context, req CopyMoveReq) error
	Put(ctx context.Context, req PutReq) error
	ServeFile(ctx context.Context, req ServeFileReq) error
}
