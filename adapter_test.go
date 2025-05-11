// Copyright 2023 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package webdav

import (
	"context"
	"io"
	"os"
	"strings"
	"time"
)

// 一个用于测试的零时间常量
var zeroTime = time.Time{}

// 适配器让测试代码可以继续使用旧的 FileSystem 接口方法
type fsAdapter struct {
	fs FileSystem
}

// Stat 适配方法，使用 List 实现
func (a *fsAdapter) Stat(ctx context.Context, name string) (os.FileInfo, error) {
	if name == "/" {
		// 根目录特殊处理
		return &fileInfoAdapter{
			info: &simpleFileInfo{
				name:    "/",
				size:    0,
				mode:    os.ModeDir | 0777,
				modTime: zeroTime,
				mime:    "httpd/unix-directory",
			},
		}, nil
	}

	// 获取父目录列表
	parent := parentDir(name)
	infos, err := a.fs.List(ctx, parent)
	if err != nil {
		return nil, err
	}

	// 查找指定文件
	base := basename(name)
	for _, info := range infos {
		if info.Name() == base {
			return &fileInfoAdapter{info: info}, nil
		}
	}

	return nil, os.ErrNotExist
}

// OpenFile 适配方法，使用 Download 和 Put 实现
func (a *fsAdapter) OpenFile(ctx context.Context, name string, flag int, perm os.FileMode) (File, error) {
	// 检查目录是否存在
	if flag&os.O_CREATE != 0 {
		parent := parentDir(name)
		_, err := a.Stat(ctx, parent)
		if err != nil && !os.IsNotExist(err) {
			return nil, err
		}
	}

	// 检查文件是否存在
	fi, statErr := a.Stat(ctx, name)

	// 处理目录情况
	if statErr == nil && fi.IsDir() {
		if flag&(os.O_WRONLY|os.O_RDWR) != 0 {
			return nil, os.ErrPermission
		}
		return &AdapterFile{
			name:    name,
			isDir:   true,
			modTime: fi.ModTime(),
			size:    0,
			mode:    fi.Mode(),
			fs:      a.fs,
			ctx:     ctx,
		}, nil
	}

	// 处理新建文件
	if os.IsNotExist(statErr) && flag&os.O_CREATE != 0 {
		// 创建空文件
		err := a.fs.Put(ctx, name, &memFileBody{
			info: &memFileInfo{
				name:    basename(name),
				size:    0,
				mode:    perm,
				modTime: zeroTime,
				mime:    getMimeType(name),
			},
			data: []byte{},
		})
		if err != nil {
			return nil, err
		}
		return &AdapterFile{
			name:    name,
			isDir:   false,
			modTime: zeroTime,
			size:    0,
			mode:    perm,
			fs:      a.fs,
			ctx:     ctx,
		}, nil
	}

	if statErr != nil {
		return nil, statErr
	}

	// 处理读取文件
	if flag&os.O_RDONLY != 0 || flag&os.O_RDWR != 0 {
		body, err := a.fs.Download(ctx, name)
		if err != nil {
			return nil, err
		}

		// 读取数据
		data, err := io.ReadAll(body)
		if err != nil {
			return nil, err
		}

		if closer, ok := body.(io.Closer); ok {
			closer.Close()
		}

		return &AdapterFile{
			name:    name,
			isDir:   false,
			modTime: fi.ModTime(),
			size:    fi.Size(),
			mode:    fi.Mode(),
			data:    data,
			fs:      a.fs,
			ctx:     ctx,
		}, nil
	}

	// 处理写入文件
	if flag&os.O_WRONLY != 0 || flag&os.O_RDWR != 0 {
		var data []byte
		if flag&os.O_TRUNC == 0 && (flag&os.O_RDWR != 0 || flag&os.O_APPEND != 0) {
			// 如果不是截断模式，需要先读取现有内容
			body, err := a.fs.Download(ctx, name)
			if err != nil {
				return nil, err
			}

			data, err = io.ReadAll(body)
			if err != nil {
				return nil, err
			}

			if closer, ok := body.(io.Closer); ok {
				closer.Close()
			}
		}

		return &AdapterFile{
			name:    name,
			isDir:   false,
			modTime: fi.ModTime(),
			size:    fi.Size(),
			mode:    fi.Mode(),
			data:    data,
			fs:      a.fs,
			ctx:     ctx,
		}, nil
	}

	return nil, os.ErrInvalid
}

// RemoveAll 适配方法，使用 Remove 实现
func (a *fsAdapter) RemoveAll(ctx context.Context, name string) error {
	return a.fs.Remove(ctx, name)
}

// Rename 适配方法，直接调用底层实现
func (a *fsAdapter) Rename(ctx context.Context, oldName, newName string) error {
	return a.fs.Rename(ctx, oldName, newName)
}

// 辅助函数，获取父目录
func parentDir(name string) string {
	parent := slashClean(name)
	parent = strings.TrimPrefix(parent, "/")

	idx := strings.LastIndex(parent, "/")
	if idx <= 0 {
		return "/"
	}

	parent = parent[:idx]
	if parent == "" {
		return "/"
	}
	return "/" + parent
}

// 辅助函数，获取文件名
func basename(name string) string {
	name = slashClean(name)
	idx := strings.LastIndex(name, "/")
	if idx < 0 {
		return name
	}
	if idx == 0 {
		return name[1:]
	}
	return name[idx+1:]
}

// 将 FileInfo 接口适配为 os.FileInfo
type fileInfoAdapter struct {
	info FileInfo
}

func (a *fileInfoAdapter) Name() string {
	return a.info.Name()
}

func (a *fileInfoAdapter) Size() int64 {
	return a.info.Size()
}

func (a *fileInfoAdapter) Mode() os.FileMode {
	return a.info.Mode()
}

func (a *fileInfoAdapter) ModTime() time.Time {
	return a.info.ModTime()
}

func (a *fileInfoAdapter) IsDir() bool {
	return a.info.IsDir()
}

func (a *fileInfoAdapter) Sys() interface{} {
	return nil
}

// 内存文件实现，用于测试
type AdapterFile struct {
	name    string
	isDir   bool
	modTime time.Time
	size    int64
	mode    os.FileMode
	data    []byte
	pos     int
	fs      FileSystem
	ctx     context.Context
}

func (f *AdapterFile) Close() error {
	if !f.isDir && f.data != nil {
		// 将更改写回文件系统
		err := f.fs.Put(f.ctx, f.name, &memFileBody{
			info: &memFileInfo{
				name:    basename(f.name),
				size:    int64(len(f.data)),
				mode:    f.mode,
				modTime: time.Now(),
				mime:    getMimeType(f.name),
			},
			data: f.data,
		})
		if err != nil {
			return err
		}
	}
	return nil
}

func (f *AdapterFile) Read(p []byte) (n int, err error) {
	if f.isDir {
		return 0, os.ErrInvalid
	}

	if f.pos >= len(f.data) {
		return 0, io.EOF
	}

	n = copy(p, f.data[f.pos:])
	f.pos += n
	return n, nil
}

func (f *AdapterFile) Readdir(count int) ([]os.FileInfo, error) {
	if !f.isDir {
		return nil, os.ErrInvalid
	}

	// 获取目录内容
	infos, err := f.fs.List(f.ctx, f.name)
	if err != nil {
		return nil, err
	}

	// 转换为 os.FileInfo
	result := make([]os.FileInfo, len(infos))
	for i, info := range infos {
		result[i] = &fileInfoAdapter{info: info}
	}

	if count <= 0 || count > len(result) {
		return result, nil
	}

	return result[:count], nil
}

func (f *AdapterFile) Seek(offset int64, whence int) (int64, error) {
	if f.isDir {
		return 0, os.ErrInvalid
	}

	var newPos int64
	switch whence {
	case io.SeekStart:
		newPos = offset
	case io.SeekCurrent:
		newPos = int64(f.pos) + offset
	case io.SeekEnd:
		newPos = int64(len(f.data)) + offset
	default:
		return 0, os.ErrInvalid
	}

	if newPos < 0 {
		return 0, os.ErrInvalid
	}

	f.pos = int(newPos)
	return newPos, nil
}

func (f *AdapterFile) Stat() (os.FileInfo, error) {
	if f.isDir {
		return &fileInfoAdapter{
			info: &simpleFileInfo{
				name:    basename(f.name),
				size:    0,
				mode:    f.mode,
				modTime: f.modTime,
				mime:    "httpd/unix-directory",
			},
		}, nil
	}

	return &fileInfoAdapter{
		info: &simpleFileInfo{
			name:    basename(f.name),
			size:    int64(len(f.data)),
			mode:    f.mode,
			modTime: f.modTime,
			mime:    getMimeType(f.name),
		},
	}, nil
}

func (f *AdapterFile) Write(p []byte) (n int, err error) {
	if f.isDir {
		return 0, os.ErrInvalid
	}

	if f.pos > len(f.data) {
		// 如果位置超出了当前数据长度，填充零值
		newData := make([]byte, f.pos, f.pos+len(p))
		copy(newData, f.data)
		f.data = newData
	}

	// 如果是追加模式，将位置设到末尾
	if f.pos == 0 && len(f.data) > 0 {
		f.pos = len(f.data)
	}

	// 确保有足够的空间
	if f.pos+len(p) > len(f.data) {
		newData := make([]byte, f.pos+len(p))
		copy(newData, f.data)
		f.data = newData
	}

	// 写入数据
	n = copy(f.data[f.pos:], p)
	f.pos += n
	f.size = int64(len(f.data))
	return n, nil
}

// AdapterFileSystem 是一个特殊的适配器接口，它表示完整的 FileSystem 接口加上测试所需的旧接口方法
type AdapterFileSystem interface {
	FileSystem
	OpenFile(ctx context.Context, name string, flag int, perm os.FileMode) (File, error)
	RemoveAll(ctx context.Context, name string) error
	Stat(ctx context.Context, name string) (os.FileInfo, error)
}

func adaptFileSystem(fs FileSystem) AdapterFileSystem {
	// 创建一个适配器实例，将旧 FileSystem 接口实现适配回新 FileSystem 接口
	return &adapterFS{
		original: fs,
		adapter:  &fsAdapter{fs: fs},
	}
}

// adapterFS 是一个特殊的适配器，它通过 fsAdapter 提供对 FileSystem 接口的兼容实现
// 同时保留对原始 FileSystem 的访问，以便可以直接调用新接口方法
type adapterFS struct {
	original FileSystem // 原始的 FileSystem 实现
	adapter  *fsAdapter // 适配回旧接口的适配器
}

// 实现 FileSystem 接口的方法
func (a *adapterFS) List(ctx context.Context, path string) ([]FileInfo, error) {
	return a.original.List(ctx, path)
}

func (a *adapterFS) Mkdir(ctx context.Context, path string, perm os.FileMode) error {
	return a.original.Mkdir(ctx, path, perm)
}

func (a *adapterFS) Remove(ctx context.Context, path string) error {
	return a.original.Remove(ctx, path)
}

func (a *adapterFS) Rename(ctx context.Context, oldPath, newName string) error {
	return a.original.Rename(ctx, oldPath, newName)
}

func (a *adapterFS) Copy(ctx context.Context, src, dst string) error {
	return a.original.Copy(ctx, src, dst)
}

func (a *adapterFS) Download(ctx context.Context, path string) (FileBody, error) {
	return a.original.Download(ctx, path)
}

func (a *adapterFS) Put(ctx context.Context, path string, file FileBody) error {
	return a.original.Put(ctx, path, file)
}

// 实现旧接口的方法
func (a *adapterFS) OpenFile(ctx context.Context, name string, flag int, perm os.FileMode) (File, error) {
	return a.adapter.OpenFile(ctx, name, flag, perm)
}

func (a *adapterFS) RemoveAll(ctx context.Context, name string) error {
	return a.adapter.RemoveAll(ctx, name)
}

func (a *adapterFS) Stat(ctx context.Context, name string) (os.FileInfo, error) {
	return a.adapter.Stat(ctx, name)
}
