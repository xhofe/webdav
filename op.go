package webdav

import (
	"context"
	"net/http"
	"os"
	"path"
	"path/filepath"
)

func _copyFiles(ctx context.Context, fs FS, src, dst string, overwrite bool, depth int) (status int, err error) {
	// check if src exists
	srcObj, err := fs.Get(ctx, GetReq{
		Path:        src,
		WithContent: false,
	})
	if err != nil {
		if os.IsNotExist(err) {
			return http.StatusNotFound, err
		}
		return http.StatusInternalServerError, err
	}
	created := false
	// check if dst exists
	if _, err = fs.Get(ctx, GetReq{
		Path:        dst,
		WithContent: false,
	}); err != nil {
		if os.IsNotExist(err) {
			created = true
		} else {
			return http.StatusForbidden, err
		}
	} else {
		if !overwrite {
			return http.StatusPreconditionFailed, os.ErrExist
		}
	}
	// if src is a directory and depth is 0, mkdir only
	if srcObj.IsDir() && depth == 0 {
		if err := fs.MkdirAll(ctx, MkdirReq{
			DirPath: dst,
			Mode:    srcObj.Mode(),
		}); err != nil {
			return http.StatusForbidden, err
		}
	} else {
		// call fs.CopyAll
		if err := fs.CopyAll(ctx, CopyMoveReq{
			SrcPath:   src,
			DstPath:   dst,
			Overwrite: overwrite,
			Depth:     depth,
		}); err != nil {
			return http.StatusForbidden, err
		}
	}
	if created {
		return http.StatusCreated, nil
	}
	return http.StatusNoContent, nil
}

func _moveFiles(ctx context.Context, fs FS, src, dst string, overwrite bool) (status int, err error) {
	created := false
	if _, err := fs.Get(ctx, GetReq{
		Path:        dst,
		WithContent: false,
	}); err != nil {
		if !os.IsNotExist(err) {
			return http.StatusForbidden, err
		}
		created = true
	} else if overwrite {
		// Section 9.9.3 says that "If a resource exists at the destination
		// and the Overwrite header is "T", then prior to performing the move,
		// the server must perform a DELETE with "Depth: infinity" on the
		// destination resource.
		// we don't need to call fs.RemoveAll, because the MoveAll will do it
	} else {
		return http.StatusPreconditionFailed, os.ErrExist
	}
	if err := fs.MoveAll(ctx, CopyMoveReq{
		SrcPath:   src,
		DstPath:   dst,
		Overwrite: overwrite,
		Depth:     infiniteDepth,
	}); err != nil {
		return http.StatusForbidden, err
	}
	if created {
		return http.StatusCreated, nil
	}
	return http.StatusNoContent, nil
}

type walkFn func(reqPath string, info ObjInfo, err error) error

func _walkFS(ctx context.Context, fs FS, depth int, name string, info ObjInfo, walkFn walkFn) error {
	// This implementation is based on Walk's code in the standard path/filepath package.
	err := walkFn(name, info, nil)
	if err != nil {
		if info.IsDir() && err == filepath.SkipDir {
			return nil
		}
		return err
	}
	if !info.IsDir() || depth == 0 {
		return nil
	}
	if depth == 1 {
		depth = 0
	}

	// Read directory names.
	// f, err := fs.OpenFile(ctx, name, os.O_RDONLY, 0)
	// if err != nil {
	// 	return walkFn(name, info, err)
	// }
	// fileInfos, err := f.Readdir(0)
	// f.Close()
	fileInfos, err := fs.List(ctx, ListReq{
		DirPath: name,
	})
	if err != nil {
		return walkFn(name, info, err)
	}

	for _, fileInfo := range fileInfos {
		filename := path.Join(name, fileInfo.Name())
		// fileInfo, err := fs.Stat(ctx, filename)
		if err != nil {
			if err := walkFn(filename, fileInfo, err); err != nil && err != filepath.SkipDir {
				return err
			}
		} else {
			err = _walkFS(ctx, fs, depth, filename, fileInfo, walkFn)
			if err != nil {
				if !fileInfo.IsDir() || err != filepath.SkipDir {
					return err
				}
			}
		}
	}
	return nil
}
