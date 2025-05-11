// Copyright 2015 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package webdav

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"time"
)

// Proppatch describes a property update instruction as defined in RFC 4918.
// See http://www.webdav.org/specs/rfc4918.html#METHOD_PROPPATCH
type Proppatch struct {
	// Remove specifies whether this patch removes properties. If it does not
	// remove them, it sets them.
	Remove bool
	// Props contains the properties to be set or removed.
	Props []Property
}

// Propstat describes a XML propstat element as defined in RFC 4918.
// See http://www.webdav.org/specs/rfc4918.html#ELEMENT_propstat
type Propstat struct {
	// Props contains the properties for which Status applies.
	Props []Property

	// Status defines the HTTP status code of the properties in Prop.
	// Allowed values include, but are not limited to the WebDAV status
	// code extensions for HTTP/1.1.
	// http://www.webdav.org/specs/rfc4918.html#status.code.extensions.to.http11
	Status int

	// XMLError contains the XML representation of the optional error element.
	// XML content within this field must not rely on any predefined
	// namespace declarations or prefixes. If empty, the XML error element
	// is omitted.
	XMLError string

	// ResponseDescription contains the contents of the optional
	// responsedescription field. If empty, the XML element is omitted.
	ResponseDescription string
}

// makePropstats returns a slice containing those of x and y whose Props slice
// is non-empty. If both are empty, it returns a slice containing an otherwise
// zero Propstat whose HTTP status code is 200 OK.
func makePropstats(x, y Propstat) []Propstat {
	pstats := make([]Propstat, 0, 2)
	if len(x.Props) != 0 {
		pstats = append(pstats, x)
	}
	if len(y.Props) != 0 {
		pstats = append(pstats, y)
	}
	if len(pstats) == 0 {
		pstats = append(pstats, Propstat{
			Status: http.StatusOK,
		})
	}
	return pstats
}

// DeadPropsHolder holds the dead properties of a resource.
//
// Dead properties are those properties that are explicitly defined. In
// comparison, live properties, such as DAV:getcontentlength, are implicitly
// defined by the underlying resource, and cannot be explicitly overridden or
// removed. See the Terminology section of
// http://www.webdav.org/specs/rfc4918.html#rfc.section.3
//
// There is a whitelist of the names of live properties. This package handles
// all live properties, and will only pass non-whitelisted names to the Patch
// method of DeadPropsHolder implementations.
type DeadPropsHolder interface {
	// DeadProps returns a copy of the dead properties held.
	DeadProps() (map[xml.Name]Property, error)

	// Patch patches the dead properties held.
	//
	// Patching is atomic; either all or no patches succeed. It returns (nil,
	// non-nil) if an internal server error occurred, otherwise the Propstats
	// collectively contain one Property for each proposed patch Property. If
	// all patches succeed, Patch returns a slice of length one and a Propstat
	// element with a 200 OK HTTP status code. If none succeed, for reasons
	// other than an internal server error, no Propstat has status 200 OK.
	//
	// For more details on when various HTTP status codes apply, see
	// http://www.webdav.org/specs/rfc4918.html#PROPPATCH-status
	Patch([]Proppatch) ([]Propstat, error)
}

// liveProps contains all supported, protected DAV: properties.
var liveProps = map[xml.Name]struct {
	// findFn implements the propfind function of this property. If nil,
	// it indicates a hidden property.
	findFn func(context.Context, FileSystem, LockSystem, string, FileInfo) (string, error)
	// dir is true if the property applies to directories.
	dir bool
}{
	{Space: "DAV:", Local: "resourcetype"}: {
		findFn: findResourceType,
		dir:    true,
	},
	{Space: "DAV:", Local: "displayname"}: {
		findFn: findDisplayName,
		dir:    true,
	},
	{Space: "DAV:", Local: "getcontentlength"}: {
		findFn: findContentLength,
		dir:    false,
	},
	{Space: "DAV:", Local: "getlastmodified"}: {
		findFn: findLastModified,
		// http://webdav.org/specs/rfc4918.html#PROPERTY_getlastmodified
		// suggests that getlastmodified should only apply to GETable
		// resources, and this package does not support GET on directories.
		//
		// Nonetheless, some WebDAV clients expect child directories to be
		// sortable by getlastmodified date, so this value is true, not false.
		// See golang.org/issue/15334.
		dir: true,
	},
	{Space: "DAV:", Local: "creationdate"}: {
		findFn: nil,
		dir:    false,
	},
	{Space: "DAV:", Local: "getcontentlanguage"}: {
		findFn: nil,
		dir:    false,
	},
	{Space: "DAV:", Local: "getcontenttype"}: {
		findFn: findContentType,
		dir:    false,
	},
	{Space: "DAV:", Local: "getetag"}: {
		findFn: findETag,
		// findETag implements ETag as the concatenated hex values of a file's
		// modification time and size. This is not a reliable synchronization
		// mechanism for directories, so we do not advertise getetag for DAV
		// collections.
		dir: false,
	},

	// TODO: The lockdiscovery property requires LockSystem to list the
	// active locks on a resource.
	{Space: "DAV:", Local: "lockdiscovery"}: {},
	{Space: "DAV:", Local: "supportedlock"}: {
		findFn: findSupportedLock,
		dir:    true,
	},
}

// TODO(nigeltao) merge props and allprop?

// props returns the status of the properties named pnames for resource name.
//
// Each Propstat has a unique status and each property name will only be part
// of one Propstat element.
func props(ctx context.Context, fs FileSystem, ls LockSystem, name string, pnames []xml.Name) ([]Propstat, error) {
	// 检查文件是否存在
	var fileInfo FileInfo
	var deadProps map[xml.Name]Property

	if name == "/" {
		// 处理根目录的特殊情况
		fileInfo = &simpleFileInfo{
			name:    "/",
			size:    0,
			mode:    os.ModeDir | 0777,
			modTime: time.Now(),
			mime:    "httpd/unix-directory",
		}
	} else {
		// 获取文件信息
		parentDir := path.Dir(name)
		fileInfos, err := fs.List(ctx, parentDir)
		if err != nil {
			return nil, err
		}

		found := false
		for _, info := range fileInfos {
			if info.Name() == path.Base(name) {
				fileInfo = info
				found = true
				break
			}
		}

		if !found {
			return nil, os.ErrNotExist
		}
	}

	// 尝试下载文件以获取 DeadProps（如果支持）
	if !fileInfo.IsDir() {
		body, err := fs.Download(ctx, name)
		if err == nil {
			if dph, ok := body.(DeadPropsHolder); ok {
				deadProps, err = dph.DeadProps()
				if err != nil {
					return nil, err
				}
			}
			if closer, ok := body.(io.Closer); ok {
				closer.Close()
			}
		}
	}

	// 如果文件是目录，尝试通过 List 获取更多信息
	if fileInfo.IsDir() {
		// 对于目录，我们不尝试获取 DeadProps
		// 因为目录通常没有关联的实际文件内容
	}

	// 处理属性
	okPnames := make([]xml.Name, 0, len(pnames))
	unknownPnames := make([]xml.Name, 0, len(pnames))
	for _, pn := range pnames {
		if _, ok := liveProps[pn]; ok {
			okPnames = append(okPnames, pn)
			continue
		}
		if deadProps != nil {
			if _, ok := deadProps[pn]; ok {
				okPnames = append(okPnames, pn)
				continue
			}
		}
		unknownPnames = append(unknownPnames, pn)
	}

	// 创建属性结果
	failedPS := Propstat{Status: http.StatusNotFound}
	successPS := Propstat{Status: http.StatusOK}

	// 添加已知属性
	for _, pn := range okPnames {
		if prop := liveProps[pn]; prop.findFn != nil && (prop.dir || !fileInfo.IsDir()) {
			innerXML, err := prop.findFn(ctx, fs, ls, name, fileInfo)
			if err != nil {
				return nil, err
			}
			successPS.Props = append(successPS.Props, Property{
				XMLName:  pn,
				InnerXML: []byte(innerXML),
			})
		} else if deadProps != nil {
			if p, ok := deadProps[pn]; ok {
				successPS.Props = append(successPS.Props, p)
			}
		}
	}

	// 添加未知属性作为失败项
	for _, pn := range unknownPnames {
		failedPS.Props = append(failedPS.Props, Property{
			XMLName: pn,
		})
	}

	return makePropstats(successPS, failedPS), nil
}

// propnames returns the property names defined for resource name.
func propnames(ctx context.Context, fs FileSystem, ls LockSystem, name string) ([]xml.Name, error) {
	// 检查文件是否存在
	var fileInfo FileInfo
	var deadProps map[xml.Name]Property

	if name == "/" {
		// 处理根目录的特殊情况
		fileInfo = &simpleFileInfo{
			name:    "/",
			size:    0,
			mode:    os.ModeDir | 0777,
			modTime: time.Now(),
			mime:    "httpd/unix-directory",
		}
	} else {
		// 获取文件信息
		parentDir := path.Dir(name)
		fileInfos, err := fs.List(ctx, parentDir)
		if err != nil {
			return nil, err
		}

		found := false
		for _, info := range fileInfos {
			if info.Name() == path.Base(name) {
				fileInfo = info
				found = true
				break
			}
		}

		if !found {
			return nil, os.ErrNotExist
		}

		// 如果是非目录文件，获取其内容以处理 DeadProps
		if !fileInfo.IsDir() {
			file, err := fs.Download(ctx, name)
			if err != nil {
				return nil, err
			}

			// 处理 DeadProps
			if dph, ok := file.(DeadPropsHolder); ok {
				deadProps, err = dph.DeadProps()
				if err != nil {
					// 确保关闭文件
					if closer, ok := file.(io.Closer); ok {
						closer.Close()
					}
					return nil, err
				}
			}

			// 确保关闭文件
			if closer, ok := file.(io.Closer); ok {
				closer.Close()
			}
		}
	}

	isDir := fileInfo.IsDir()

	pnames := make([]xml.Name, 0, len(liveProps)+len(deadProps))
	for pn, prop := range liveProps {
		if prop.findFn != nil && (prop.dir || !isDir) {
			pnames = append(pnames, pn)
		}
	}
	for pn := range deadProps {
		pnames = append(pnames, pn)
	}
	return pnames, nil
}

// allprop returns the properties defined for resource name and the properties
// named in include.
//
// Note that RFC 4918 defines 'allprop' to return the DAV: properties defined
// within the RFC plus dead properties. Other live properties should only be
// returned if they are named in 'include'.
//
// See http://www.webdav.org/specs/rfc4918.html#METHOD_PROPFIND
func allprop(ctx context.Context, fs FileSystem, ls LockSystem, name string, include []xml.Name) ([]Propstat, error) {
	pnames, err := propnames(ctx, fs, ls, name)
	if err != nil {
		return nil, err
	}
	// Add the properties named in include, avoiding duplicates.
	set := make(map[xml.Name]bool)
	for _, pn := range pnames {
		set[pn] = true
	}
	for _, pn := range include {
		if !set[pn] {
			pnames = append(pnames, pn)
		}
	}
	return props(ctx, fs, ls, name, pnames)
}

// patch applies the patches to the resource named in the request.
func patch(ctx context.Context, fs FileSystem, ls LockSystem, name string, patches []Proppatch) ([]Propstat, error) {
	// 处理 DeadProps
	file, err := fs.Download(ctx, name)
	if err != nil {
		return nil, err
	}
	defer func() {
		if closer, ok := file.(io.Closer); ok {
			closer.Close()
		}
	}()

	dph, ok := file.(DeadPropsHolder)
	if !ok {
		return nil, errNoDeadPropsHolder
	}

	// 检查请求的属性中哪些是活属性（不能被修改）
	pstat := Propstat{Status: http.StatusForbidden}
	for _, patch := range patches {
		for _, p := range patch.Props {
			if _, ok := liveProps[p.XMLName]; ok {
				pstat.Props = append(pstat.Props, Property{XMLName: p.XMLName})
			}
		}
	}

	// 修补死属性
	pstats, err := dph.Patch(patches)
	if err != nil {
		return nil, err
	}

	// 返回结果
	if len(pstat.Props) == 0 {
		return pstats, nil
	}
	return append(pstats, pstat), nil
}

var errNoDeadPropsHolder = errors.New("webdav: no DeadPropsHolder")

// escapeXML writes s to w with reserved xml characters (<, >, &) escaped.
func escapeXML(s string) string {
	var buf bytes.Buffer
	xml.Escape(&buf, []byte(s))
	return buf.String()
}

// findResourceType returns the XML representation of the resource type of
// file, or returns an error if it's not a recognized resource type.
func findResourceType(ctx context.Context, fs FileSystem, ls LockSystem, name string, fi FileInfo) (string, error) {
	if fi.IsDir() {
		return `<collection xmlns="DAV:"/>`, nil
	}
	return "", nil
}

// findDisplayName returns the XML representation of the display name of file.
func findDisplayName(ctx context.Context, fs FileSystem, ls LockSystem, name string, fi FileInfo) (string, error) {
	if name == "/" {
		// Hide the real name of a possibly prefixed root directory.
		return escapeXML(string(filepath.Separator)), nil
	}
	return escapeXML(fi.Name()), nil
}

// findContentLength returns the XML representation of the content length of file.
func findContentLength(ctx context.Context, fs FileSystem, ls LockSystem, name string, fi FileInfo) (string, error) {
	return strconv.FormatInt(fi.Size(), 10), nil
}

// findLastModified returns the XML representation of the last modification time of file.
func findLastModified(ctx context.Context, fs FileSystem, ls LockSystem, name string, fi FileInfo) (string, error) {
	return fi.ModTime().UTC().Format(http.TimeFormat), nil
}

// ContentTyper is an optional interface for a FileInfo to report its MIME Content-Type.
type ContentTyper interface {
	// ContentType returns the content type for the file.
	//
	// If this returns error ErrNotImplemented then the error will
	// be ignored and the base implementation will be used
	// instead.
	ContentType(ctx context.Context) (string, error)
}

// errNotImplemented implies that a method is not supported.
var errNotImplemented = errors.New("webdav: not implemented")

// findContentType returns the XML representation of the content type of file.
func findContentType(ctx context.Context, fs FileSystem, ls LockSystem, name string, fi FileInfo) (string, error) {
	if fi.IsDir() {
		return "httpd/unix-directory", nil
	}

	if cter, ok := fi.(ContentTyper); ok {
		ct, err := cter.ContentType(ctx)
		if err == nil {
			return ct, nil
		}
		if err != errNotImplemented {
			return "", err
		}
	}

	// 使用 FileInfo 的 Mime 方法
	if mime := fi.Mime(); mime != "" {
		return mime, nil
	}

	// 返回一个默认的MIME类型
	return "application/octet-stream", nil
}

// ETager is an optional interface for a FileInfo to report its ETag.
type ETager interface {
	// ETag returns an ETag for the file.  This should be of the
	// form "value" or W/"value"
	//
	// If this returns error ErrNotImplemented then the error will
	// be ignored and the base implementation will be used
	// instead.
	ETag(ctx context.Context) (string, error)
}

// findETag returns the XML representation of the ETag of file.
func findETag(ctx context.Context, fs FileSystem, ls LockSystem, name string, fi FileInfo) (string, error) {
	if etager, ok := fi.(ETager); ok {
		et, err := etager.ETag(ctx)
		if err == nil {
			return et, nil
		}
		if err != errNotImplemented {
			return "", err
		}
	}

	// 如果ETager接口不可用，则生成基本的ETag
	modTime := fi.ModTime()
	size := fi.Size()
	return fmt.Sprintf(`"%x%x"`, modTime.UnixNano(), size), nil
}

// findSupportedLock returns the XML representation of the supported locks for file.
func findSupportedLock(ctx context.Context, fs FileSystem, ls LockSystem, name string, fi FileInfo) (string, error) {
	return `` +
		`<lockentry xmlns="DAV:">` +
		`<lockscope><exclusive/></lockscope>` +
		`<locktype><write/></locktype>` +
		`</lockentry>` +
		`<lockentry xmlns="DAV:">` +
		`<lockscope><shared/></lockscope>` +
		`<locktype><write/></locktype>` +
		`</lockentry>`, nil
}
