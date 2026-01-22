package webdav

import (
	"context"
	"io"
	"io/fs"
	"mime"
	"net/http"
	"os"
	"path"
	"time"
)

type FSAdapter interface {
	FS
	RawFS() FileSystem
}

type fsAdapter struct {
	fs FileSystem
}

// Get implements FS.
func (f *fsAdapter) Get(ctx context.Context, req GetReq) (Obj, error) {
	// Get file info
	fi, err := f.fs.OpenFile(ctx, req.Path, os.O_RDONLY, 0)
	if err != nil {
		return nil, err
	}

	if mem, ok := fi.(*memFile); ok {
		return mem, nil
	}

	fileInfo, err := fi.Stat()
	if err != nil {
		return nil, err
	}

	// Create Obj
	o := &object{
		name:      path.Base(req.Path),
		size:      fileInfo.Size(),
		mode:      fileInfo.Mode(),
		modTime:   fileInfo.ModTime(),
		isDir:     fileInfo.IsDir(),
		createdAt: fileInfo.ModTime(), // Use ModTime as CreatedAt
		mime:      f.getMimeType(req.Path, fileInfo),
	}

	return o, nil
}

// List implements FS.
func (f *fsAdapter) List(ctx context.Context, req ListReq) ([]Obj, error) {
	// Open directory
	file, err := f.fs.OpenFile(ctx, req.DirPath, os.O_RDONLY, 0)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	// Read directory content
	fileInfos, err := file.Readdir(-1)
	if err != nil {
		return nil, err
	}

	// Convert to ObjInfo list
	objInfos := make([]Obj, len(fileInfos))
	for i, fi := range fileInfos {
		childPath := path.Join(req.DirPath, fi.Name())
		if mem, ok := fi.(*memFile); ok {
			objInfos[i] = mem
			continue
		}
		objInfos[i] = &object{
			name:      fi.Name(),
			size:      fi.Size(),
			mode:      fi.Mode(),
			modTime:   fi.ModTime(),
			isDir:     fi.IsDir(),
			createdAt: fi.ModTime(),
			mime:      f.getMimeType(childPath, fi),
		}
	}

	return objInfos, nil
}

// MkdirAll implements FS.
func (f *fsAdapter) MkdirAll(ctx context.Context, req MkdirReq) error {
	return f.fs.Mkdir(ctx, req.DirPath, req.Mode)
}

// RemoveAll implements FS.
func (f *fsAdapter) RemoveAll(ctx context.Context, req RemoveReq) error {
	return f.fs.RemoveAll(ctx, req.Path)
}

// Rename implements FS.
func (f *fsAdapter) Rename(ctx context.Context, req RenameReq) error {
	return f.fs.Rename(ctx, req.OldPath, req.NewPath)
}

// CopyAll implements FS.
func (f *fsAdapter) CopyAll(ctx context.Context, req CopyMoveReq) error {
	// Use copyFiles function in file.go
	_, err := copyFiles(ctx, f.fs, req.SrcPath, req.DstPath, req.Overwrite, req.Depth, 0)
	return err
}

// MoveAll implements FS.
func (f *fsAdapter) MoveAll(ctx context.Context, req CopyMoveReq) error {
	// Use moveFiles function in file.go
	_, err := moveFiles(ctx, f.fs, req.SrcPath, req.DstPath, req.Overwrite)
	return err
}

// Put implements FS.
func (f *fsAdapter) Put(ctx context.Context, req PutReq) error {
	// Create or open file for writing
	file, err := f.fs.OpenFile(ctx, req.Path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0644)
	if err != nil {
		return err
	}
	defer file.Close()

	// Copy content
	_, err = io.Copy(file, req.Content)
	if err != nil {
		return err
	}

	return nil
}

// ServeFile implements FS.
func (f *fsAdapter) ServeFile(ctx context.Context, req ServeFileReq) error {
	// Check if req.File implements io.ReadSeeker
	if seeker, ok := req.File.(io.ReadSeeker); ok {
		// Use standard library's http.ServeContent
		http.ServeContent(req.RespWriter, req.Req, req.File.Name(), req.File.ModTime(), seeker)
		return nil
	}

	// If not supported Seek, copy content directly
	req.RespWriter.Header().Set("Content-Type", req.File.Mime())
	file, err := f.fs.OpenFile(ctx, req.Path, os.O_RDONLY, 0)
	if err != nil {
		return err
	}
	defer file.Close()

	_, err = io.Copy(req.RespWriter, file)
	return err
}

// getMimeType get file MIME type
func (f *fsAdapter) getMimeType(filePath string, fileInfo os.FileInfo) string {
	if fileInfo.IsDir() {
		return ""
	}

	// First try to get MIME type by file extension
	ext := path.Ext(filePath)
	mimeType := mime.TypeByExtension(ext)
	if mimeType != "" {
		return mimeType
	}

	// Default return application/octet-stream
	return "application/octet-stream"
}

// RawFS implements FSAdapter.
func (f *fsAdapter) RawFS() FileSystem {
	return f.fs
}

func AdaptFS(fs FileSystem) FSAdapter {
	return &fsAdapter{fs: fs}
}

// CreatedAt implements Obj.
func (f *memFile) CreatedAt() time.Time {
	return f.n.modTime
}

// IsDir implements Obj.
func (f *memFile) IsDir() bool {
	return f.n.mode.IsDir()
}

// Mime implements Obj.
func (f *memFile) Mime() string {
	// Read a chunk to decide between utf-8 text and binary.
	var buf [512]byte
	n, err := io.ReadFull(f, buf[:])
	if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
		return ""
	}
	ctype := http.DetectContentType(buf[:n])
	// Rewind file.
	_, _ = f.Seek(0, io.SeekStart)
	return ctype
}

// ModTime implements Obj.
func (f *memFile) ModTime() time.Time {
	return f.n.modTime
}

// Mode implements Obj.
func (f *memFile) Mode() fs.FileMode {
	return f.n.mode
}

// Name implements Obj.
func (f *memFile) Name() string {
	return f.nameSnapshot
}

// Size implements Obj.
func (f *memFile) Size() int64 {
	return int64(len(f.n.data))
}

// Sys implements Obj.
func (f *memFile) Sys() any {
	return nil
}

var _ Obj = (*memFile)(nil)
