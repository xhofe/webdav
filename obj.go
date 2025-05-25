package webdav

import (
	"io"
	"io/fs"
	"time"
)

type ObjInfo interface {
	fs.FileInfo
	Mime() string
	CreatedAt() time.Time
}

type Obj interface {
	ObjInfo
	URL() string
	io.ReadCloser
}

type objectInfo struct {
	mime      string
	createdAt time.Time
	isDir     bool
	size      int64
	modTime   time.Time
	mode      fs.FileMode
	name      string
}

// CreatedAt implements ObjInfo.
func (o *objectInfo) CreatedAt() time.Time {
	return o.createdAt
}

// IsDir implements ObjInfo.
func (o *objectInfo) IsDir() bool {
	return o.isDir
}

// Mime implements ObjInfo.
func (o *objectInfo) Mime() string {
	return o.mime
}

// ModTime implements ObjInfo.
func (o *objectInfo) ModTime() time.Time {
	return o.modTime
}

// Mode implements ObjInfo.
func (o *objectInfo) Mode() fs.FileMode {
	return o.mode
}

// Name implements ObjInfo.
func (o *objectInfo) Name() string {
	return o.name
}

// Size implements ObjInfo.
func (o *objectInfo) Size() int64 {
	return o.size
}

// Sys implements ObjInfo.
func (o *objectInfo) Sys() any {
	return nil
}

var _ ObjInfo = &objectInfo{}

type object struct {
	ObjInfo
	io.ReadCloser
	url string
}

// URL implements Obj.
func (o *object) URL() string {
	return o.url
}

var _ Obj = &object{}
