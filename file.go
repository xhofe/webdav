// Copyright 2014 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package webdav

import (
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// slashClean is equivalent to but slightly more efficient than
// path.Clean("/" + name).
func slashClean(name string) string {
	if name == "" || name[0] != '/' {
		name = "/" + name
	}
	return path.Clean(name)
}

type FileInfo interface {
	Name() string
	Size() int64
	ModTime() time.Time
	Mode() os.FileMode
	Mime() string
	IsDir() bool
}

type FileBody interface {
	FileInfo
	URL() string
	io.Reader
}

// A FileSystem implements access to a collection of named files. The elements
// in a file path are separated by slash ('/', U+002F) characters, regardless
// of host operating system convention.
//
// Each method has the same semantics as the os package's function of the same
// name.
//
// Note that the os.Rename documentation says that "OS-specific restrictions
// might apply". In particular, whether or not renaming a file or directory
// overwriting another existing file or directory is an error is OS-dependent.
type FileSystem interface {
	List(ctx context.Context, path string) ([]FileInfo, error)
	Mkdir(ctx context.Context, path string, perm os.FileMode) error
	Remove(ctx context.Context, path string) error
	Rename(ctx context.Context, oldPath, newName string) error
	Copy(ctx context.Context, src, dst string) error
	Download(ctx context.Context, path string) (FileBody, error)
	Put(ctx context.Context, path string, file FileBody) error
}

// A File is returned by a FileSystem's OpenFile method and can be served by a
// Handler.
//
// A File may optionally implement the DeadPropsHolder interface, if it can
// load and save dead properties.
type File interface {
	http.File
	io.Writer
}

// A Dir implements FileSystem using the native file system restricted to a
// specific directory tree.
//
// While the FileSystem.OpenFile method takes '/'-separated paths, a Dir's
// string value is a filename on the native file system, not a URL, so it is
// separated by filepath.Separator, which isn't necessarily '/'.
//
// An empty Dir is treated as ".".
type Dir string

func (d Dir) resolve(name string) string {
	// This implementation is based on Dir.Open's code in the standard net/http package.
	if filepath.Separator != '/' && strings.IndexRune(name, filepath.Separator) >= 0 ||
		strings.Contains(name, "\x00") {
		return ""
	}
	dir := string(d)
	if dir == "" {
		dir = "."
	}
	return filepath.Join(dir, filepath.FromSlash(slashClean(name)))
}

func (d Dir) List(ctx context.Context, name string) ([]FileInfo, error) {
	if name = d.resolve(name); name == "" {
		return nil, os.ErrNotExist
	}
	f, err := os.Open(name)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	stat, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !stat.IsDir() {
		return nil, os.ErrInvalid
	}

	infos, err := f.Readdir(-1)
	if err != nil {
		return nil, err
	}

	fileInfos := make([]FileInfo, len(infos))
	for i, info := range infos {
		fileInfos[i] = &dirFileInfo{info, ""}
	}
	return fileInfos, nil
}

func (d Dir) Mkdir(ctx context.Context, name string, perm os.FileMode) error {
	if name = d.resolve(name); name == "" {
		return os.ErrNotExist
	}
	return os.Mkdir(name, perm)
}

func (d Dir) Remove(ctx context.Context, name string) error {
	if name = d.resolve(name); name == "" {
		return os.ErrNotExist
	}
	if name == filepath.Clean(string(d)) {
		// Prohibit removing the virtual root directory.
		return os.ErrInvalid
	}
	return os.RemoveAll(name)
}

func (d Dir) Rename(ctx context.Context, oldName, newName string) error {
	if oldName = d.resolve(oldName); oldName == "" {
		return os.ErrNotExist
	}
	if newName = d.resolve(newName); newName == "" {
		return os.ErrNotExist
	}
	if root := filepath.Clean(string(d)); root == oldName || root == newName {
		// Prohibit renaming from or to the virtual root directory.
		return os.ErrInvalid
	}
	return os.Rename(oldName, newName)
}

func (d Dir) Copy(ctx context.Context, src, dst string) error {
	srcPath := d.resolve(src)
	if srcPath == "" {
		return os.ErrNotExist
	}

	dstPath := d.resolve(dst)
	if dstPath == "" {
		return os.ErrNotExist
	}

	srcInfo, err := os.Stat(srcPath)
	if err != nil {
		return err
	}

	if srcInfo.IsDir() {
		return fmt.Errorf("directory copy not supported")
	}

	srcFile, err := os.Open(srcPath)
	if err != nil {
		return err
	}
	defer srcFile.Close()

	dstFile, err := os.Create(dstPath)
	if err != nil {
		return err
	}
	defer dstFile.Close()

	_, err = io.Copy(dstFile, srcFile)
	return err
}

func (d Dir) Download(ctx context.Context, name string) (FileBody, error) {
	if name = d.resolve(name); name == "" {
		return nil, os.ErrNotExist
	}

	info, err := os.Stat(name)
	if err != nil {
		return nil, err
	}

	if info.IsDir() {
		return nil, os.ErrInvalid
	}

	file, err := os.Open(name)
	if err != nil {
		return nil, err
	}

	return &dirFileBody{file, &dirFileInfo{info, ""}, ""}, nil
}

func (d Dir) Put(ctx context.Context, name string, file FileBody) error {
	if name = d.resolve(name); name == "" {
		return os.ErrNotExist
	}

	dst, err := os.Create(name)
	if err != nil {
		return err
	}
	defer dst.Close()

	_, err = io.Copy(dst, file)
	return err
}

// 为本地文件系统实现 FileInfo 接口的适配器
type dirFileInfo struct {
	os.FileInfo
	mime string
}

func (fi *dirFileInfo) Mime() string {
	if fi.mime == "" {
		// 可以基于文件扩展名获取 MIME 类型
		fi.mime = getMimeType(fi.Name())
	}
	return fi.mime
}

// 为本地文件系统实现 FileBody 接口的适配器
type dirFileBody struct {
	file *os.File
	info *dirFileInfo
	url  string
}

func (fb *dirFileBody) Name() string {
	return fb.info.Name()
}

func (fb *dirFileBody) Size() int64 {
	return fb.info.Size()
}

func (fb *dirFileBody) ModTime() time.Time {
	return fb.info.ModTime()
}

func (fb *dirFileBody) Mode() os.FileMode {
	return fb.info.Mode()
}

func (fb *dirFileBody) Mime() string {
	return fb.info.Mime()
}

func (fb *dirFileBody) IsDir() bool {
	return fb.info.IsDir()
}

func (fb *dirFileBody) URL() string {
	return fb.url
}

func (fb *dirFileBody) Read(p []byte) (n int, err error) {
	return fb.file.Read(p)
}

func (fb *dirFileBody) Close() error {
	return fb.file.Close()
}

// 简单的 MIME 类型检测函数
func getMimeType(name string) string {
	ext := strings.ToLower(filepath.Ext(name))
	switch ext {
	case ".html", ".htm":
		return "text/html"
	case ".css":
		return "text/css"
	case ".js":
		return "application/javascript"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".gif":
		return "image/gif"
	case ".png":
		return "image/png"
	case ".pdf":
		return "application/pdf"
	case ".txt":
		return "text/plain"
	case ".xml":
		return "application/xml"
	case ".json":
		return "application/json"
	default:
		return "application/octet-stream"
	}
}

// NewMemFS returns a new in-memory FileSystem implementation.
func NewMemFS() FileSystem {
	return &memFS{
		root: memFSNode{
			children: make(map[string]*memFSNode),
			mode:     0660 | os.ModeDir,
			modTime:  time.Now(),
		},
	}
}

// A memFS implements FileSystem, storing all metadata and actual file data
// in-memory. No limits on filesystem size are used, so it is not recommended
// this be used where the clients are untrusted.
//
// Concurrent access is permitted. The tree structure is protected by a mutex,
// and each node's contents and metadata are protected by a per-node mutex.
//
// TODO: Enforce file permissions.
type memFS struct {
	mu   sync.Mutex
	root memFSNode
}

// TODO: clean up and rationalize the walk/find code.

// walk walks the directory tree for the fullname, calling f at each step. If f
// returns an error, the walk will be aborted and return that same error.
//
// dir is the directory at that step, frag is the name fragment, and final is
// whether it is the final step. For example, walking "/foo/bar/x" will result
// in 3 calls to f:
//   - "/", "foo", false
//   - "/foo/", "bar", false
//   - "/foo/bar/", "x", true
//
// The frag argument will be empty only if dir is the root node and the walk
// ends at that root node.
func (fs *memFS) walk(op, fullname string, f func(dir *memFSNode, frag string, final bool) error) error {
	original := fullname
	fullname = slashClean(fullname)

	// Strip any leading "/"s to make fullname a relative path, as the walk
	// starts at fs.root.
	if fullname[0] == '/' {
		fullname = fullname[1:]
	}
	dir := &fs.root

	for {
		frag, remaining := fullname, ""
		i := strings.IndexRune(fullname, '/')
		final := i < 0
		if !final {
			frag, remaining = fullname[:i], fullname[i+1:]
		}
		if frag == "" && dir != &fs.root {
			panic("webdav: empty path fragment for a clean path")
		}
		if err := f(dir, frag, final); err != nil {
			return &os.PathError{
				Op:   op,
				Path: original,
				Err:  err,
			}
		}
		if final {
			break
		}
		child := dir.children[frag]
		if child == nil {
			return &os.PathError{
				Op:   op,
				Path: original,
				Err:  os.ErrNotExist,
			}
		}
		if !child.mode.IsDir() {
			return &os.PathError{
				Op:   op,
				Path: original,
				Err:  os.ErrInvalid,
			}
		}
		dir, fullname = child, remaining
	}
	return nil
}

// find returns the parent of the named node and the relative name fragment
// from the parent to the child. For example, if finding "/foo/bar/baz" then
// parent will be the node for "/foo/bar" and frag will be "baz".
//
// If the fullname names the root node, then parent, frag and err will be zero.
//
// find returns an error if the parent does not already exist or the parent
// isn't a directory, but it will not return an error per se if the child does
// not already exist. The error returned is either nil or an *os.PathError
// whose Op is op.
func (fs *memFS) find(op, fullname string) (parent *memFSNode, frag string, err error) {
	err = fs.walk(op, fullname, func(parent0 *memFSNode, frag0 string, final bool) error {
		if !final {
			return nil
		}
		if frag0 != "" {
			parent, frag = parent0, frag0
		}
		return nil
	})
	return parent, frag, err
}

func (fs *memFS) List(ctx context.Context, name string) ([]FileInfo, error) {
	fs.mu.Lock()
	defer fs.mu.Unlock()

	dir, frag, err := fs.find("list", name)
	if err != nil {
		return nil, err
	}

	var node *memFSNode
	if dir == nil {
		// 处理根目录的情况
		node = &fs.root
	} else if frag == "" {
		node = dir
	} else {
		node = dir.children[frag]
		if node == nil {
			return nil, os.ErrNotExist
		}
	}

	node.mu.Lock()
	defer node.mu.Unlock()

	if !node.mode.IsDir() {
		return nil, os.ErrInvalid
	}

	fileInfos := make([]FileInfo, 0, len(node.children))
	for name, child := range node.children {
		child.mu.Lock()
		fileInfos = append(fileInfos, &memFileInfo{
			name:    name,
			size:    int64(len(child.data)),
			mode:    child.mode,
			modTime: child.modTime,
			mime:    "",
		})
		child.mu.Unlock()
	}

	return fileInfos, nil
}

func (fs *memFS) Mkdir(ctx context.Context, name string, perm os.FileMode) error {
	fs.mu.Lock()
	defer fs.mu.Unlock()

	dir, frag, err := fs.find("mkdir", name)
	if err != nil {
		return err
	}
	if dir == nil {
		// We can't create the root directory as it always exists.
		return os.ErrExist
	}
	if frag == "" {
		return os.ErrExist
	}
	if _, ok := dir.children[frag]; ok {
		return os.ErrExist
	}
	dir.children[frag] = &memFSNode{
		children: make(map[string]*memFSNode),
		mode:     perm | os.ModeDir,
		modTime:  time.Now(),
	}
	return nil
}

func (fs *memFS) Remove(ctx context.Context, name string) error {
	fs.mu.Lock()
	defer fs.mu.Unlock()

	dir, frag, err := fs.find("remove", name)
	if err != nil {
		return err
	}
	if dir == nil || frag == "" {
		// We can't remove the root directory.
		return os.ErrInvalid
	}
	if _, ok := dir.children[frag]; !ok {
		return os.ErrNotExist
	}
	delete(dir.children, frag)
	return nil
}

func (fs *memFS) Rename(ctx context.Context, oldName, newName string) error {
	fs.mu.Lock()
	defer fs.mu.Unlock()

	oldDir, oldFrag, err := fs.find("rename", oldName)
	if err != nil {
		return err
	}
	if oldDir == nil || oldFrag == "" {
		// We can't rename the root directory.
		return os.ErrInvalid
	}

	newDir, newFrag, err := fs.find("rename", newName)
	if err != nil {
		return err
	}
	if newDir == nil || newFrag == "" {
		// We can't rename to become the root directory.
		return os.ErrInvalid
	}

	n := oldDir.children[oldFrag]
	if n == nil {
		return os.ErrNotExist
	}
	if _, ok := newDir.children[newFrag]; ok {
		return os.ErrExist
	}

	delete(oldDir.children, oldFrag)
	newDir.children[newFrag] = n
	return nil
}

func (fs *memFS) Copy(ctx context.Context, src, dst string) error {
	fs.mu.Lock()
	defer fs.mu.Unlock()

	srcDir, srcFrag, err := fs.find("copy", src)
	if err != nil {
		return err
	}

	var srcNode *memFSNode
	if srcDir == nil {
		if src == "/" {
			return os.ErrInvalid // 不能复制根目录
		}
		return os.ErrNotExist
	} else if srcFrag == "" {
		srcNode = srcDir
	} else {
		srcNode = srcDir.children[srcFrag]
		if srcNode == nil {
			return os.ErrNotExist
		}
	}

	srcNode.mu.Lock()
	defer srcNode.mu.Unlock()

	// 只支持复制文件，不支持目录
	if srcNode.mode.IsDir() {
		return fmt.Errorf("directory copy not supported")
	}

	dstDir, dstFrag, err := fs.find("copy", dst)
	if err != nil {
		return err
	}

	if dstDir == nil || dstFrag == "" {
		return os.ErrInvalid
	}

	// 检查目标是否已存在
	if _, ok := dstDir.children[dstFrag]; ok {
		return os.ErrExist
	}

	// 创建新节点并复制数据
	dstNode := &memFSNode{
		children: make(map[string]*memFSNode),
		data:     make([]byte, len(srcNode.data)),
		mode:     srcNode.mode,
		modTime:  time.Now(),
	}
	copy(dstNode.data, srcNode.data)

	dstDir.children[dstFrag] = dstNode
	return nil
}

func (fs *memFS) Download(ctx context.Context, name string) (FileBody, error) {
	fs.mu.Lock()
	defer fs.mu.Unlock()

	dir, frag, err := fs.find("download", name)
	if err != nil {
		return nil, err
	}

	var node *memFSNode
	if dir == nil {
		if name == "/" {
			return nil, os.ErrInvalid // 根目录不能下载
		}
		return nil, os.ErrNotExist
	} else if frag == "" {
		node = dir
	} else {
		node = dir.children[frag]
		if node == nil {
			return nil, os.ErrNotExist
		}
	}

	node.mu.Lock()
	defer node.mu.Unlock()

	if node.mode.IsDir() {
		return nil, os.ErrInvalid
	}

	// 创建一个内存文件体
	return &memFileBody{
		node: node,
		info: &memFileInfo{
			name:    filepath.Base(name),
			size:    int64(len(node.data)),
			mode:    node.mode,
			modTime: node.modTime,
			mime:    getMimeType(name),
		},
		data: node.data,
	}, nil
}

func (fs *memFS) Put(ctx context.Context, name string, file FileBody) error {
	fs.mu.Lock()
	defer fs.mu.Unlock()

	dir, frag, err := fs.find("put", name)
	if err != nil {
		return err
	}

	if dir == nil || frag == "" {
		return os.ErrInvalid
	}

	// 读取所有数据
	data, err := io.ReadAll(file)
	if err != nil {
		return err
	}

	// 创建或更新节点
	node, ok := dir.children[frag]
	if !ok {
		node = &memFSNode{
			children: make(map[string]*memFSNode),
			mode:     0666,
		}
		dir.children[frag] = node
	}

	node.mu.Lock()
	defer node.mu.Unlock()

	node.modTime = time.Now()
	node.data = data

	return nil
}

// A memFSNode represents a single entry in the in-memory filesystem and also
// implements os.FileInfo.
type memFSNode struct {
	// children is protected by memFS.mu.
	children map[string]*memFSNode

	mu        sync.Mutex
	data      []byte
	mode      os.FileMode
	modTime   time.Time
	deadProps map[xml.Name]Property
}

func (n *memFSNode) stat(name string) *memFileInfo {
	n.mu.Lock()
	defer n.mu.Unlock()
	return &memFileInfo{
		name:    name,
		size:    int64(len(n.data)),
		mode:    n.mode,
		modTime: n.modTime,
	}
}

func (n *memFSNode) DeadProps() (map[xml.Name]Property, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if len(n.deadProps) == 0 {
		return nil, nil
	}
	ret := make(map[xml.Name]Property, len(n.deadProps))
	for k, v := range n.deadProps {
		ret[k] = v
	}
	return ret, nil
}

func (n *memFSNode) Patch(patches []Proppatch) ([]Propstat, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	pstat := Propstat{Status: http.StatusOK}
	for _, patch := range patches {
		for _, p := range patch.Props {
			pstat.Props = append(pstat.Props, Property{XMLName: p.XMLName})
			if patch.Remove {
				delete(n.deadProps, p.XMLName)
				continue
			}
			if n.deadProps == nil {
				n.deadProps = map[xml.Name]Property{}
			}
			n.deadProps[p.XMLName] = p
		}
	}
	return []Propstat{pstat}, nil
}

type memFileInfo struct {
	name    string
	size    int64
	mode    os.FileMode
	modTime time.Time
	mime    string
}

func (f *memFileInfo) Name() string       { return f.name }
func (f *memFileInfo) Size() int64        { return f.size }
func (f *memFileInfo) Mode() os.FileMode  { return f.mode }
func (f *memFileInfo) ModTime() time.Time { return f.modTime }
func (f *memFileInfo) IsDir() bool        { return f.mode.IsDir() }
func (f *memFileInfo) Sys() interface{}   { return nil }
func (f *memFileInfo) Mime() string {
	if f.mime == "" {
		f.mime = getMimeType(f.name)
	}
	return f.mime
}

// A memFile is a File implementation for a memFSNode. It is a per-file (not
// per-node) read/write position, and a snapshot of the memFS' tree structure
// (a node's name and children) for that node.
type memFile struct {
	n                *memFSNode
	nameSnapshot     string
	childrenSnapshot []os.FileInfo
	// pos is protected by n.mu.
	pos int
}

// A *memFile implements the optional DeadPropsHolder interface.
var _ DeadPropsHolder = (*memFile)(nil)

func (f *memFile) DeadProps() (map[xml.Name]Property, error)     { return f.n.DeadProps() }
func (f *memFile) Patch(patches []Proppatch) ([]Propstat, error) { return f.n.Patch(patches) }

func (f *memFile) Close() error {
	return nil
}

func (f *memFile) Read(p []byte) (int, error) {
	f.n.mu.Lock()
	defer f.n.mu.Unlock()
	if f.n.mode.IsDir() {
		return 0, os.ErrInvalid
	}
	if f.pos >= len(f.n.data) {
		return 0, io.EOF
	}
	n := copy(p, f.n.data[f.pos:])
	f.pos += n
	return n, nil
}

func (f *memFile) Readdir(count int) ([]os.FileInfo, error) {
	f.n.mu.Lock()
	defer f.n.mu.Unlock()
	if !f.n.mode.IsDir() {
		return nil, os.ErrInvalid
	}
	old := f.pos
	if old >= len(f.childrenSnapshot) {
		// The os.File Readdir docs say that at the end of a directory,
		// the error is io.EOF if count > 0 and nil if count <= 0.
		if count > 0 {
			return nil, io.EOF
		}
		return nil, nil
	}
	if count > 0 {
		f.pos += count
		if f.pos > len(f.childrenSnapshot) {
			f.pos = len(f.childrenSnapshot)
		}
	} else {
		f.pos = len(f.childrenSnapshot)
		old = 0
	}
	return f.childrenSnapshot[old:f.pos], nil
}

func (f *memFile) Seek(offset int64, whence int) (int64, error) {
	f.n.mu.Lock()
	defer f.n.mu.Unlock()
	npos := f.pos
	// TODO: How to handle offsets greater than the size of system int?
	switch whence {
	case io.SeekStart:
		npos = int(offset)
	case io.SeekCurrent:
		npos += int(offset)
	case io.SeekEnd:
		npos = len(f.n.data) + int(offset)
	default:
		npos = -1
	}
	if npos < 0 {
		return 0, os.ErrInvalid
	}
	f.pos = npos
	return int64(f.pos), nil
}

func (f *memFile) Stat() (os.FileInfo, error) {
	return f.n.stat(f.nameSnapshot), nil
}

func (f *memFile) Write(p []byte) (int, error) {
	lenp := len(p)
	f.n.mu.Lock()
	defer f.n.mu.Unlock()

	if f.n.mode.IsDir() {
		return 0, os.ErrInvalid
	}
	if f.pos < len(f.n.data) {
		n := copy(f.n.data[f.pos:], p)
		f.pos += n
		p = p[n:]
	} else if f.pos > len(f.n.data) {
		// Write permits the creation of holes, if we've seek'ed past the
		// existing end of file.
		if f.pos <= cap(f.n.data) {
			oldLen := len(f.n.data)
			f.n.data = f.n.data[:f.pos]
			hole := f.n.data[oldLen:]
			for i := range hole {
				hole[i] = 0
			}
		} else {
			d := make([]byte, f.pos, f.pos+len(p))
			copy(d, f.n.data)
			f.n.data = d
		}
	}

	if len(p) > 0 {
		// We should only get here if f.pos == len(f.n.data).
		f.n.data = append(f.n.data, p...)
		f.pos = len(f.n.data)
	}
	f.n.modTime = time.Now()
	return lenp, nil
}

// 实现 FileBody 接口的内存文件体
type memFileBody struct {
	node *memFSNode
	info *memFileInfo
	data []byte
	pos  int
}

func (f *memFileBody) Name() string {
	return f.info.Name()
}

func (f *memFileBody) Size() int64 {
	return f.info.Size()
}

func (f *memFileBody) ModTime() time.Time {
	return f.info.ModTime()
}

func (f *memFileBody) Mode() os.FileMode {
	return f.info.Mode()
}

func (f *memFileBody) Mime() string {
	return f.info.Mime()
}

func (f *memFileBody) IsDir() bool {
	return f.info.IsDir()
}

func (f *memFileBody) URL() string {
	return ""
}

func (f *memFileBody) Read(p []byte) (int, error) {
	if f.pos >= len(f.data) {
		return 0, io.EOF
	}
	n := copy(p, f.data[f.pos:])
	f.pos += n
	return n, nil
}

func copyProps(dst, src File) error {
	d, ok := dst.(DeadPropsHolder)
	if !ok {
		return nil
	}
	s, ok := src.(DeadPropsHolder)
	if !ok {
		return nil
	}
	m, err := s.DeadProps()
	if err != nil {
		return err
	}
	props := make([]Property, 0, len(m))
	for _, prop := range m {
		props = append(props, prop)
	}
	_, err = d.Patch([]Proppatch{{Props: props}})
	return err
}
