// Copyright 2014 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package webdav provides a WebDAV server implementation.
package webdav // import "golang.org/x/net/webdav"

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
)

type Handler struct {
	// Prefix is the URL path prefix to strip from WebDAV resource paths.
	Prefix string
	// FileSystem is the virtual file system.
	FileSystem FileSystem
	// LockSystem is the lock management system.
	LockSystem LockSystem
	// Logger is an optional error logger. If non-nil, it will be called
	// for all HTTP requests.
	Logger func(*http.Request, error)
}

func (h *Handler) stripPrefix(p string) (string, int, error) {
	if h.Prefix == "" {
		return p, http.StatusOK, nil
	}
	if r := strings.TrimPrefix(p, h.Prefix); len(r) < len(p) {
		return r, http.StatusOK, nil
	}
	return p, http.StatusNotFound, errPrefixMismatch
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	status, err := http.StatusBadRequest, errUnsupportedMethod
	if h.FileSystem == nil {
		status, err = http.StatusInternalServerError, errNoFileSystem
	} else if h.LockSystem == nil {
		status, err = http.StatusInternalServerError, errNoLockSystem
	} else {
		switch r.Method {
		case "OPTIONS":
			status, err = h.handleOptions(w, r)
		case "GET", "HEAD", "POST":
			status, err = h.handleGetHeadPost(w, r)
		case "DELETE":
			status, err = h.handleDelete(w, r)
		case "PUT":
			status, err = h.handlePut(w, r)
		case "MKCOL":
			status, err = h.handleMkcol(w, r)
		case "COPY", "MOVE":
			status, err = h.handleCopyMove(w, r)
		case "LOCK":
			status, err = h.handleLock(w, r)
		case "UNLOCK":
			status, err = h.handleUnlock(w, r)
		case "PROPFIND":
			status, err = h.handlePropfind(w, r)
		case "PROPPATCH":
			status, err = h.handleProppatch(w, r)
		}
	}

	if status != 0 {
		w.WriteHeader(status)
		if status != http.StatusNoContent {
			w.Write([]byte(StatusText(status)))
		}
	}
	if h.Logger != nil {
		h.Logger(r, err)
	}
}

func (h *Handler) lock(now time.Time, root string) (token string, status int, err error) {
	token, err = h.LockSystem.Create(now, LockDetails{
		Root:      root,
		Duration:  infiniteTimeout,
		ZeroDepth: true,
	})
	if err != nil {
		if err == ErrLocked {
			return "", StatusLocked, err
		}
		return "", http.StatusInternalServerError, err
	}
	return token, 0, nil
}

func (h *Handler) confirmLocks(r *http.Request, src, dst string) (release func(), status int, err error) {
	hdr := r.Header.Get("If")
	if hdr == "" {
		// An empty If header means that the client hasn't previously created locks.
		// Even if this client doesn't care about locks, we still need to check that
		// the resources aren't locked by another client, so we create temporary
		// locks that would conflict with another client's locks. These temporary
		// locks are unlocked at the end of the HTTP request.
		now, srcToken, dstToken := time.Now(), "", ""
		if src != "" {
			srcToken, status, err = h.lock(now, src)
			if err != nil {
				return nil, status, err
			}
		}
		if dst != "" {
			dstToken, status, err = h.lock(now, dst)
			if err != nil {
				if srcToken != "" {
					h.LockSystem.Unlock(now, srcToken)
				}
				return nil, status, err
			}
		}

		return func() {
			if dstToken != "" {
				h.LockSystem.Unlock(now, dstToken)
			}
			if srcToken != "" {
				h.LockSystem.Unlock(now, srcToken)
			}
		}, 0, nil
	}

	ih, ok := parseIfHeader(hdr)
	if !ok {
		return nil, http.StatusBadRequest, errInvalidIfHeader
	}
	// ih is a disjunction (OR) of ifLists, so any ifList will do.
	for _, l := range ih.lists {
		lsrc := l.resourceTag
		if lsrc == "" {
			lsrc = src
		} else {
			u, err := url.Parse(lsrc)
			if err != nil {
				continue
			}
			if u.Host != r.Host {
				continue
			}
			lsrc, status, err = h.stripPrefix(u.Path)
			if err != nil {
				return nil, status, err
			}
		}
		release, err = h.LockSystem.Confirm(time.Now(), lsrc, dst, l.conditions...)
		if err == ErrConfirmationFailed {
			continue
		}
		if err != nil {
			return nil, http.StatusInternalServerError, err
		}
		return release, 0, nil
	}
	// Section 10.4.1 says that "If this header is evaluated and all state lists
	// fail, then the request must fail with a 412 (Precondition Failed) status."
	// We follow the spec even though the cond_put_corrupt_token test case from
	// the litmus test warns on seeing a 412 instead of a 423 (Locked).
	return nil, http.StatusPreconditionFailed, ErrLocked
}

func (h *Handler) handleOptions(w http.ResponseWriter, r *http.Request) (status int, err error) {
	reqPath, status, err := h.stripPrefix(r.URL.Path)
	if err != nil {
		return status, err
	}
	ctx := r.Context()
	allow := "OPTIONS, LOCK, PUT, MKCOL"

	// 检查路径是否为目录
	fileInfos, err := h.FileSystem.List(ctx, path.Dir(reqPath))
	if err == nil {
		for _, info := range fileInfos {
			if info.Name() == path.Base(reqPath) {
				if info.IsDir() {
					allow = "OPTIONS, LOCK, DELETE, PROPPATCH, COPY, MOVE, UNLOCK, PROPFIND"
				} else {
					allow = "OPTIONS, LOCK, GET, HEAD, POST, DELETE, PROPPATCH, COPY, MOVE, UNLOCK, PROPFIND, PUT"
				}
				break
			}
		}
	}

	w.Header().Set("Allow", allow)
	// http://www.webdav.org/specs/rfc4918.html#dav.compliance.classes
	w.Header().Set("DAV", "1, 2")
	// http://msdn.microsoft.com/en-au/library/cc250217.aspx
	w.Header().Set("MS-Author-Via", "DAV")
	return 0, nil
}

func (h *Handler) handleGetHeadPost(w http.ResponseWriter, r *http.Request) (status int, err error) {
	reqPath, status, err := h.stripPrefix(r.URL.Path)
	if err != nil {
		return status, err
	}
	// TODO: check locks for read-only access??
	ctx := r.Context()

	fileBody, err := h.FileSystem.Download(ctx, reqPath)
	if err != nil {
		return http.StatusNotFound, err
	}

	defer func() {
		if closer, ok := fileBody.(io.Closer); ok {
			closer.Close()
		}
	}()

	// 检查是否有 URL，有则进行 302 重定向
	if url := fileBody.URL(); url != "" {
		w.Header().Set("Location", url)
		return http.StatusFound, nil
	}

	// 计算 ETag
	modTime := fileBody.ModTime()
	size := fileBody.Size()
	etag := fmt.Sprintf(`"%x%x"`, modTime.UnixNano(), size)

	w.Header().Set("ETag", etag)
	w.Header().Set("Content-Type", fileBody.Mime())

	// 设置内容长度
	w.Header().Set("Content-Length", fmt.Sprintf("%d", fileBody.Size()))

	// 设置最后修改时间
	w.Header().Set("Last-Modified", fileBody.ModTime().UTC().Format(http.TimeFormat))

	// 如果是 HEAD 请求，不需要发送内容
	if r.Method == "HEAD" {
		return http.StatusOK, nil
	}

	// 发送文件内容
	_, err = io.Copy(w, fileBody)
	if err != nil {
		return http.StatusInternalServerError, err
	}

	return 0, nil
}

func (h *Handler) handleDelete(w http.ResponseWriter, r *http.Request) (status int, err error) {
	reqPath, status, err := h.stripPrefix(r.URL.Path)
	if err != nil {
		return status, err
	}
	release, status, err := h.confirmLocks(r, reqPath, "")
	if err != nil {
		return status, err
	}
	defer release()

	ctx := r.Context()

	// TODO: return MultiStatus where appropriate.

	// 检查文件是否存在
	fileInfos, err := h.FileSystem.List(ctx, path.Dir(reqPath))
	if err != nil {
		return http.StatusMethodNotAllowed, err
	}

	exists := false
	for _, info := range fileInfos {
		if info.Name() == path.Base(reqPath) {
			exists = true
			break
		}
	}

	if !exists {
		return http.StatusNotFound, os.ErrNotExist
	}

	if err := h.FileSystem.Remove(ctx, reqPath); err != nil {
		return http.StatusMethodNotAllowed, err
	}
	return http.StatusNoContent, nil
}

func (h *Handler) handlePut(w http.ResponseWriter, r *http.Request) (status int, err error) {
	reqPath, status, err := h.stripPrefix(r.URL.Path)
	if err != nil {
		return status, err
	}
	release, status, err := h.confirmLocks(r, reqPath, "")
	if err != nil {
		return status, err
	}
	defer release()

	// TODO: support the If-Match, If-None-Match headers? See RFC 7232.

	// 创建一个 FileBody 实现来包装请求体
	fileBody := &requestFileBody{
		Reader: r.Body,
		info: &simpleFileInfo{
			name:    path.Base(reqPath),
			size:    r.ContentLength,
			mode:    0666,
			modTime: time.Now(),
			mime:    r.Header.Get("Content-Type"),
		},
	}

	ctx := r.Context()
	err = h.FileSystem.Put(ctx, reqPath, fileBody)
	if err != nil {
		return http.StatusMethodNotAllowed, err
	}
	return http.StatusCreated, nil
}

func (h *Handler) handleMkcol(w http.ResponseWriter, r *http.Request) (status int, err error) {
	reqPath, status, err := h.stripPrefix(r.URL.Path)
	if err != nil {
		return status, err
	}
	release, status, err := h.confirmLocks(r, reqPath, "")
	if err != nil {
		return status, err
	}
	defer release()

	ctx := r.Context()

	if r.ContentLength > 0 {
		return http.StatusUnsupportedMediaType, nil
	}
	if err := h.FileSystem.Mkdir(ctx, reqPath, 0777); err != nil {
		if os.IsNotExist(err) {
			return http.StatusConflict, err
		}
		if os.IsExist(err) {
			return http.StatusMethodNotAllowed, err
		}
		return http.StatusMethodNotAllowed, err
	}
	return http.StatusCreated, nil
}

func (h *Handler) handleCopyMove(w http.ResponseWriter, r *http.Request) (status int, err error) {
	hdr := r.Header.Get("Destination")
	if hdr == "" {
		return http.StatusBadRequest, errInvalidDestination
	}
	u, err := url.Parse(hdr)
	if err != nil {
		return http.StatusBadRequest, errInvalidDestination
	}
	if u.Host != "" && u.Host != r.Host {
		return http.StatusBadGateway, errInvalidDestination
	}

	src, status, err := h.stripPrefix(r.URL.Path)
	if err != nil {
		return status, err
	}

	dst, status, err := h.stripPrefix(u.Path)
	if err != nil {
		return status, err
	}

	if dst == "" {
		return http.StatusBadGateway, errInvalidDestination
	}
	if dst == src {
		return http.StatusForbidden, errDestinationEqualsSource
	}

	ctx := r.Context()

	if r.Method == "COPY" {
		// Section 7.5.1 says that a COPY only needs to lock the destination,
		// not both destination and source. Section 9.8.4 says that
		// servers should ensure that locks are maintained during COPY.
		release, status, err := h.confirmLocks(r, "", dst)
		if err != nil {
			return status, err
		}
		defer release()

		// Section 9.8.3 says "The COPY method on a collection without a Depth
		// header must act as if a Depth header with value "infinity" was included".
		depth := infiniteDepth
		if hdr := r.Header.Get("Depth"); hdr != "" {
			depth = parseDepth(hdr)
			if depth != 0 && depth != infiniteDepth {
				// Section 9.8.3 says "A client may submit a Depth header on a
				// COPY on a collection with a value of "0" or "infinity"."
				return http.StatusBadRequest, errInvalidDepth
			}
		}
		return copyFiles(ctx, h.FileSystem, src, dst, r.Header.Get("Overwrite") != "F", depth, 0)
	}

	// r.Method == "MOVE"
	release, status, err := h.confirmLocks(r, src, dst)
	if err != nil {
		return status, err
	}
	defer release()

	// Section 9.9.2 says "The MOVE method on a collection must act as if
	// a "Depth: infinity" header was used on it. A client must not submit
	// a Depth header on a MOVE on a collection with any value but "infinity"."
	if hdr := r.Header.Get("Depth"); hdr != "" {
		if parseDepth(hdr) != infiniteDepth {
			return http.StatusBadRequest, errInvalidDepth
		}
	}
	return moveFiles(ctx, h.FileSystem, src, dst, r.Header.Get("Overwrite") != "F")
}

func (h *Handler) handleLock(w http.ResponseWriter, r *http.Request) (retStatus int, retErr error) {
	duration, err := parseTimeout(r.Header.Get("Timeout"))
	if err != nil {
		return http.StatusBadRequest, err
	}
	li, status, err := readLockInfo(r.Body)
	if err != nil {
		return status, err
	}

	ctx := r.Context()
	token, ld, now, created := "", LockDetails{}, time.Now(), false
	if li == (lockInfo{}) {
		// An empty lockInfo means to refresh the lock.
		ih, ok := parseIfHeader(r.Header.Get("If"))
		if !ok {
			return http.StatusBadRequest, errInvalidIfHeader
		}
		if len(ih.lists) == 1 && len(ih.lists[0].conditions) == 1 {
			token = ih.lists[0].conditions[0].Token
		}
		if token == "" {
			return http.StatusBadRequest, errInvalidLockToken
		}
		ld, err = h.LockSystem.Refresh(now, token, duration)
		if err != nil {
			if err == ErrNoSuchLock {
				return http.StatusPreconditionFailed, err
			}
			return http.StatusInternalServerError, err
		}

	} else {
		// Section 9.10.3 says that "If no Depth header is submitted on a LOCK request,
		// then the request MUST act as if a "Depth:infinity" had been submitted."
		depth := infiniteDepth
		if hdr := r.Header.Get("Depth"); hdr != "" {
			depth = parseDepth(hdr)
			if depth != 0 && depth != infiniteDepth {
				// Section 9.10.3 says that "Values other than 0 or infinity must not be
				// used with the Depth header on a LOCK method".
				return http.StatusBadRequest, errInvalidDepth
			}
		}
		reqPath, status, err := h.stripPrefix(r.URL.Path)
		if err != nil {
			return status, err
		}
		ld = LockDetails{
			Root:      reqPath,
			Duration:  duration,
			OwnerXML:  li.Owner.InnerXML,
			ZeroDepth: depth == 0,
		}
		token, err = h.LockSystem.Create(now, ld)
		if err != nil {
			if err == ErrLocked {
				return StatusLocked, err
			}
			return http.StatusInternalServerError, err
		}
		defer func() {
			if retErr != nil {
				h.LockSystem.Unlock(now, token)
			}
		}()

		// 创建资源，如果它尚不存在
		// 检查资源是否存在
		fileInfos, err := h.FileSystem.List(ctx, path.Dir(reqPath))
		exists := false
		if err == nil {
			for _, info := range fileInfos {
				if info.Name() == path.Base(reqPath) {
					exists = true
					break
				}
			}
		}

		if !exists {
			// 创建一个空文件
			emptyBody := &requestFileBody{
				Reader: strings.NewReader(""),
				info: &simpleFileInfo{
					name:    path.Base(reqPath),
					size:    0,
					mode:    0666,
					modTime: time.Now(),
					mime:    "application/octet-stream",
				},
			}
			if err := h.FileSystem.Put(ctx, reqPath, emptyBody); err != nil {
				return http.StatusInternalServerError, err
			}
			created = true
		}

		// http://www.webdav.org/specs/rfc4918.html#HEADER_Lock-Token says that the
		// Lock-Token value is a Coded-URL. We add angle brackets.
		w.Header().Set("Lock-Token", "<"+token+">")
	}

	w.Header().Set("Content-Type", "application/xml; charset=utf-8")
	if created {
		// This is "w.WriteHeader(http.StatusCreated)" and not "return
		// http.StatusCreated, nil" because we write our own (XML) response to w
		// and Handler.ServeHTTP would otherwise write "Created".
		w.WriteHeader(http.StatusCreated)
	}
	writeLockInfo(w, token, ld)
	return 0, nil
}

func (h *Handler) handleUnlock(w http.ResponseWriter, r *http.Request) (status int, err error) {
	// http://www.webdav.org/specs/rfc4918.html#HEADER_Lock-Token says that the
	// Lock-Token value is a Coded-URL. We strip its angle brackets.
	t := r.Header.Get("Lock-Token")
	if len(t) < 2 || t[0] != '<' || t[len(t)-1] != '>' {
		return http.StatusBadRequest, errInvalidLockToken
	}
	t = t[1 : len(t)-1]

	switch err = h.LockSystem.Unlock(time.Now(), t); err {
	case nil:
		return http.StatusNoContent, err
	case ErrForbidden:
		return http.StatusForbidden, err
	case ErrLocked:
		return StatusLocked, err
	case ErrNoSuchLock:
		return http.StatusConflict, err
	default:
		return http.StatusInternalServerError, err
	}
}

func (h *Handler) handlePropfind(w http.ResponseWriter, r *http.Request) (status int, err error) {
	reqPath, status, err := h.stripPrefix(r.URL.Path)
	if err != nil {
		return status, err
	}
	ctx := r.Context()

	// 检查请求路径是否存在
	var fi FileInfo
	var fileExists bool

	if reqPath == "/" {
		// 处理根路径
		fileExists = true
		fi = &simpleFileInfo{
			name:    "/",
			size:    0,
			mode:    os.ModeDir | 0777,
			modTime: time.Now(),
			mime:    "httpd/unix-directory",
		}
	} else {
		// 查找文件或目录
		fileInfos, err := h.FileSystem.List(ctx, path.Dir(reqPath))
		if err == nil {
			for _, info := range fileInfos {
				if info.Name() == path.Base(reqPath) {
					fileExists = true
					fi = info
					break
				}
			}
		}
	}

	if !fileExists {
		return http.StatusNotFound, os.ErrNotExist
	}

	depth := infiniteDepth
	if hdr := r.Header.Get("Depth"); hdr != "" {
		depth = parseDepth(hdr)
		if depth == invalidDepth {
			return http.StatusBadRequest, errInvalidDepth
		}
	}
	pf, status, err := readPropfind(r.Body)
	if err != nil {
		return status, err
	}

	mw := multistatusWriter{w: w}

	// 调整walkFn从FileInfo使用
	walkFn := func(reqPath string, info FileInfo, err error) error {
		if err != nil {
			return handlePropfindError(err, compatFileInfo{info})
		}

		var pstats []Propstat
		if pf.Propname != nil {
			pnames, err := propnames(ctx, h.FileSystem, h.LockSystem, reqPath)
			if err != nil {
				return handlePropfindError(err, compatFileInfo{info})
			}
			pstat := Propstat{Status: http.StatusOK}
			for _, xmlname := range pnames {
				pstat.Props = append(pstat.Props, Property{XMLName: xmlname})
			}
			pstats = append(pstats, pstat)
		} else if pf.Allprop != nil {
			pstats, err = allprop(ctx, h.FileSystem, h.LockSystem, reqPath, pf.Prop)
		} else {
			pstats, err = props(ctx, h.FileSystem, h.LockSystem, reqPath, pf.Prop)
		}
		if err != nil {
			return handlePropfindError(err, compatFileInfo{info})
		}
		href := path.Join(h.Prefix, reqPath)
		if href != "/" && info.IsDir() {
			href += "/"
		}
		return mw.write(makePropstatResponse(href, pstats))
	}

	// 执行自定义的目录遍历
	var walkErr error
	if fi.IsDir() {
		if walkErr = walkFn(reqPath, fi, nil); walkErr == nil && depth != 0 {
			walkErr = listWalker(ctx, h.FileSystem, reqPath, depth, walkFn)
		}
	} else {
		walkErr = walkFn(reqPath, fi, nil)
	}

	closeErr := mw.close()
	if walkErr != nil {
		return http.StatusInternalServerError, walkErr
	}
	if closeErr != nil {
		return http.StatusInternalServerError, closeErr
	}
	return 0, nil
}

// 实现目录递归函数，替代原来的 walkFS
func listWalker(ctx context.Context, fs FileSystem, name string, depth int, fn func(name string, info FileInfo, err error) error) error {
	if depth == 0 {
		return nil
	}

	// 读取目录内容
	fileInfos, err := fs.List(ctx, name)
	if err != nil {
		return fn(name, nil, err)
	}

	for _, info := range fileInfos {
		path := path.Join(name, info.Name())
		err := fn(path, info, nil)
		if err != nil {
			if err == filepath.SkipDir {
				continue
			}
			return err
		}

		if info.IsDir() && (depth == infiniteDepth || depth > 1) {
			nextDepth := depth
			if nextDepth > 0 {
				nextDepth--
			}
			if err := listWalker(ctx, fs, path, nextDepth, fn); err != nil {
				return err
			}
		}
	}

	return nil
}

// 为新的 FileInfo 接口提供兼容层，使其能够满足 os.FileInfo 接口
type compatFileInfo struct {
	FileInfo
}

func (c compatFileInfo) Sys() interface{} {
	return nil
}

func (h *Handler) handleProppatch(w http.ResponseWriter, r *http.Request) (status int, err error) {
	reqPath, status, err := h.stripPrefix(r.URL.Path)
	if err != nil {
		return status, err
	}
	release, status, err := h.confirmLocks(r, reqPath, "")
	if err != nil {
		return status, err
	}
	defer release()

	ctx := r.Context()

	// 检查文件是否存在
	fileExists := false
	if reqPath == "/" {
		fileExists = true
	} else {
		fileInfos, err := h.FileSystem.List(ctx, path.Dir(reqPath))
		if err == nil {
			for _, info := range fileInfos {
				if info.Name() == path.Base(reqPath) {
					fileExists = true
					break
				}
			}
		}
	}

	if !fileExists {
		return http.StatusNotFound, os.ErrNotExist
	}

	patches, status, err := readProppatch(r.Body)
	if err != nil {
		return status, err
	}
	pstats, err := patch(ctx, h.FileSystem, h.LockSystem, reqPath, patches)
	if err != nil {
		return http.StatusInternalServerError, err
	}
	mw := multistatusWriter{w: w}
	writeErr := mw.write(makePropstatResponse(r.URL.Path, pstats))
	closeErr := mw.close()
	if writeErr != nil {
		return http.StatusInternalServerError, writeErr
	}
	if closeErr != nil {
		return http.StatusInternalServerError, closeErr
	}
	return 0, nil
}

func makePropstatResponse(href string, pstats []Propstat) *response {
	resp := response{
		Href:     []string{(&url.URL{Path: href}).EscapedPath()},
		Propstat: make([]propstat, 0, len(pstats)),
	}
	for _, p := range pstats {
		var xmlErr *xmlError
		if p.XMLError != "" {
			xmlErr = &xmlError{InnerXML: []byte(p.XMLError)}
		}
		resp.Propstat = append(resp.Propstat, propstat{
			Status:              fmt.Sprintf("HTTP/1.1 %d %s", p.Status, StatusText(p.Status)),
			Prop:                p.Props,
			ResponseDescription: p.ResponseDescription,
			Error:               xmlErr,
		})
	}
	return &resp
}

func handlePropfindError(err error, info os.FileInfo) error {
	var skipResp error = nil
	if info != nil && info.IsDir() {
		skipResp = filepath.SkipDir
	}

	if errors.Is(err, os.ErrPermission) {
		// If the server cannot recurse into a directory because it is not allowed,
		// then there is nothing more to say about it. Just skip sending anything.
		return skipResp
	}

	if _, ok := err.(*os.PathError); ok {
		// If the file is just bad, it couldn't be a proper WebDAV resource. Skip it.
		return skipResp
	}

	// We need to be careful with other errors: there is no way to abort the xml stream
	// part way through while returning a valid PROPFIND response. Returning only half
	// the data would be misleading, but so would be returning results tainted by errors.
	// The current behaviour by returning an error here leads to the stream being aborted,
	// and the parent http server complaining about writing a spurious header. We should
	// consider further enhancing this error handling to more gracefully fail, or perhaps
	// buffer the entire response until we've walked the tree.
	return err
}

const (
	infiniteDepth = -1
	invalidDepth  = -2
)

// parseDepth maps the strings "0", "1" and "infinity" to 0, 1 and
// infiniteDepth. Parsing any other string returns invalidDepth.
//
// Different WebDAV methods have further constraints on valid depths:
//   - PROPFIND has no further restrictions, as per section 9.1.
//   - COPY accepts only "0" or "infinity", as per section 9.8.3.
//   - MOVE accepts only "infinity", as per section 9.9.2.
//   - LOCK accepts only "0" or "infinity", as per section 9.10.3.
//
// These constraints are enforced by the handleXxx methods.
func parseDepth(s string) int {
	switch s {
	case "0":
		return 0
	case "1":
		return 1
	case "infinity":
		return infiniteDepth
	}
	return invalidDepth
}

// http://www.webdav.org/specs/rfc4918.html#status.code.extensions.to.http11
const (
	StatusMulti               = 207
	StatusUnprocessableEntity = 422
	StatusLocked              = 423
	StatusFailedDependency    = 424
	StatusInsufficientStorage = 507
	StatusPreconditionFailed  = http.StatusPreconditionFailed
)

func StatusText(code int) string {
	switch code {
	case StatusMulti:
		return "Multi-Status"
	case StatusUnprocessableEntity:
		return "Unprocessable Entity"
	case StatusLocked:
		return "Locked"
	case StatusFailedDependency:
		return "Failed Dependency"
	case StatusInsufficientStorage:
		return "Insufficient Storage"
	}
	return http.StatusText(code)
}

var (
	errDestinationEqualsSource = errors.New("webdav: destination equals source")
	errDirectoryNotEmpty       = errors.New("webdav: directory not empty")
	errInvalidDepth            = errors.New("webdav: invalid depth")
	errInvalidDestination      = errors.New("webdav: invalid destination")
	errInvalidIfHeader         = errors.New("webdav: invalid If header")
	errInvalidLockInfo         = errors.New("webdav: invalid lock info")
	errInvalidLockToken        = errors.New("webdav: invalid lock token")
	errInvalidPropfind         = errors.New("webdav: invalid propfind")
	errInvalidProppatch        = errors.New("webdav: invalid proppatch")
	errInvalidResponse         = errors.New("webdav: invalid response")
	errInvalidTimeout          = errors.New("webdav: invalid timeout")
	errNoFileSystem            = errors.New("webdav: no file system")
	errNoLockSystem            = errors.New("webdav: no lock system")
	errNotADirectory           = errors.New("webdav: not a directory")
	errPrefixMismatch          = errors.New("webdav: prefix mismatch")
	errRecursionTooDeep        = errors.New("webdav: recursion too deep")
	errUnsupportedLockInfo     = errors.New("webdav: unsupported lock info")
	errUnsupportedMethod       = errors.New("webdav: unsupported method")
)

// 为了支持重定向的 FileBody 实现
type requestFileBody struct {
	io.Reader
	info FileInfo
}

func (rb *requestFileBody) Name() string {
	return rb.info.Name()
}

func (rb *requestFileBody) Size() int64 {
	return rb.info.Size()
}

func (rb *requestFileBody) ModTime() time.Time {
	return rb.info.ModTime()
}

func (rb *requestFileBody) Mode() os.FileMode {
	return rb.info.Mode()
}

func (rb *requestFileBody) Mime() string {
	return rb.info.Mime()
}

func (rb *requestFileBody) IsDir() bool {
	return rb.info.IsDir()
}

func (rb *requestFileBody) URL() string {
	return ""
}

// 简单的 FileInfo 实现
type simpleFileInfo struct {
	name    string
	size    int64
	mode    os.FileMode
	modTime time.Time
	mime    string
}

func (fi *simpleFileInfo) Name() string {
	return fi.name
}

func (fi *simpleFileInfo) Size() int64 {
	return fi.size
}

func (fi *simpleFileInfo) Mode() os.FileMode {
	return fi.mode
}

func (fi *simpleFileInfo) ModTime() time.Time {
	return fi.modTime
}

func (fi *simpleFileInfo) IsDir() bool {
	return fi.mode.IsDir()
}

func (fi *simpleFileInfo) Sys() interface{} {
	return nil
}

func (fi *simpleFileInfo) Mime() string {
	return fi.mime
}

// moveFiles 函数调整为使用新接口
func moveFiles(ctx context.Context, fs FileSystem, src, dst string, overwrite bool) (status int, err error) {
	if overwrite {
		// 检查目标是否存在，如果存在则先删除
		dstDir := path.Dir(dst)
		fileInfos, err := fs.List(ctx, dstDir)
		if err == nil {
			for _, info := range fileInfos {
				if info.Name() == path.Base(dst) {
					// 目标文件存在，需要删除
					if err := fs.Remove(ctx, dst); err != nil {
						return http.StatusForbidden, err
					}
					break
				}
			}
		}
	}

	if err := fs.Rename(ctx, src, dst); err != nil {
		return http.StatusForbidden, err
	}
	return http.StatusNoContent, nil
}

// copyFiles 函数调整为使用新接口
func copyFiles(ctx context.Context, fs FileSystem, src, dst string, overwrite bool, depth int, recursion int) (status int, err error) {
	if recursion == 1000 {
		return http.StatusInternalServerError, errRecursionTooDeep
	}

	// 首先检查源文件/目录是否存在
	srcDir := path.Dir(src)
	srcBase := path.Base(src)
	srcExists := false

	fileInfos, err := fs.List(ctx, srcDir)
	if err != nil {
		return http.StatusNotFound, err
	}

	var srcInfo FileInfo
	for _, info := range fileInfos {
		if info.Name() == srcBase {
			srcExists = true
			srcInfo = info
			break
		}
	}

	if !srcExists {
		return http.StatusNotFound, os.ErrNotExist
	}

	// 检查目标是否已存在
	dstExists := false
	dstDir := path.Dir(dst)
	dstBase := path.Base(dst)

	dstInfos, err := fs.List(ctx, dstDir)
	if err == nil {
		for _, info := range dstInfos {
			if info.Name() == dstBase {
				dstExists = true
				if !overwrite {
					return http.StatusPreconditionFailed, os.ErrExist
				}
				break
			}
		}
	}

	if dstExists && overwrite {
		if err := fs.Remove(ctx, dst); err != nil {
			return http.StatusForbidden, err
		}
	}

	if !srcInfo.Mode().IsDir() {
		// 复制文件
		return copyFile(ctx, fs, src, dst)
	} else if depth == 0 {
		// 创建目录但不复制其内容
		if err := fs.Mkdir(ctx, dst, srcInfo.Mode()); err != nil {
			return http.StatusForbidden, err
		}
		return http.StatusCreated, nil
	}

	// 深度复制目录
	if err := fs.Mkdir(ctx, dst, srcInfo.Mode()); err != nil {
		return http.StatusConflict, err
	}

	// 读取源目录内容
	children, err := fs.List(ctx, src)
	if err != nil {
		return http.StatusInternalServerError, err
	}

	// 递归复制子项
	for _, child := range children {
		childSrc := path.Join(src, child.Name())
		childDst := path.Join(dst, child.Name())
		st, err := copyFiles(ctx, fs, childSrc, childDst, overwrite, depth, recursion+1)
		if err != nil {
			return st, err
		}
	}

	return http.StatusCreated, nil
}

// 辅助函数 - 复制单个文件
func copyFile(ctx context.Context, fs FileSystem, src, dst string) (status int, err error) {
	if err := fs.Copy(ctx, src, dst); err != nil {
		return http.StatusInternalServerError, err
	}
	return http.StatusCreated, nil
}
