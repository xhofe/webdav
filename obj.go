package webdav

import (
	"io/fs"
	"time"
)

type Obj interface {
	fs.FileInfo
	Mime() string
	CreatedAt() time.Time
}

type object struct {
	mime      string
	createdAt time.Time
	isDir     bool
	size      int64
	modTime   time.Time
	mode      fs.FileMode
	name      string
}

// CreatedAt implements ObjInfo.
func (o *object) CreatedAt() time.Time {
	return o.createdAt
}

// IsDir implements ObjInfo.
func (o *object) IsDir() bool {
	return o.isDir
}

// Mime implements ObjInfo.
func (o *object) Mime() string {
	return o.mime
}

// ModTime implements ObjInfo.
func (o *object) ModTime() time.Time {
	return o.modTime
}

// Mode implements ObjInfo.
func (o *object) Mode() fs.FileMode {
	return o.mode
}

// Name implements ObjInfo.
func (o *object) Name() string {
	return o.name
}

// Size implements ObjInfo.
func (o *object) Size() int64 {
	return o.size
}

// Sys implements ObjInfo.
func (o *object) Sys() any {
	return nil
}

var _ Obj = &object{}
