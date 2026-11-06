package http

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/accounting"
	"github.com/rclone/rclone/fs/filter"
	"github.com/rclone/rclone/fs/operations"
	"github.com/rclone/rclone/lib/http/serve"
	"github.com/rclone/rclone/vfs"
)

// serveDirectFile returns false when the object needs the VFS unknown-size path.
func (s *HTTP) serveDirectFile(w http.ResponseWriter, r *http.Request, remote string, filesystem *vfs.VFS) bool {
	if !filter.GetConfig(s.ctx).InActive() {
		return false
	}
	if filepath.IsAbs(remote) || filepath.VolumeName(remote) != "" {
		http.NotFound(w, r)
		return true
	}
	for _, part := range strings.Split(remote, "/") {
		if part == "" || part == "." || part == ".." || strings.ContainsAny(part, "\\\x00") {
			http.NotFound(w, r)
			return true
		}
	}
	object, err := filesystem.Fs().NewObject(r.Context(), remote)
	if err != nil {
		if errors.Is(err, fs.ErrorObjectNotFound) || errors.Is(err, fs.ErrorDirNotFound) || errors.Is(err, fs.ErrorIsDir) {
			http.NotFound(w, r)
		} else {
			serve.Error(r.Context(), remote, w, "Failed to find file", err)
		}
		return true
	}
	if object.Size() < 0 {
		return false
	}
	w.Header().Set("Content-Length", strconv.FormatInt(object.Size(), 10))
	mimeType := fs.MimeType(r.Context(), object)
	if mimeType != "application/octet-stream" || path.Ext(remote) != "" {
		w.Header().Set("Content-Type", mimeType)
	}
	modTime := object.ModTime(r.Context())
	w.Header().Set("Last-Modified", modTime.UTC().Format(http.TimeFormat))
	if r.Method == "HEAD" {
		return true
	}
	reader := &directReadSeeker{ctx: r.Context(), object: object, chunkSize: int64(filesystem.Opt.ChunkSize)}
	defer func() {
		if err := reader.Close(); err != nil {
			fs.Errorf(remote, "Failed to close file: %v", err)
		}
	}()
	transfer := accounting.Stats(r.Context()).NewTransfer(object, nil)
	defer transfer.Done(r.Context(), nil)
	accounted, err := transfer.Account(r.Context(), reader).WithSeeker()
	if err != nil {
		serve.Error(r.Context(), remote, w, "Failed to account file", err)
		return true
	}
	http.ServeContent(w, r, remote, modTime, accounted)
	return true
}

// directReadSeeker opens a bounded backend range only when the HTTP server reads.
type directReadSeeker struct {
	ctx                         context.Context
	object                      fs.Object
	offset, chunkSize, chunkEnd int64
	in                          io.ReadCloser
}

func (r *directReadSeeker) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if err := r.ctx.Err(); err != nil {
		_ = r.Close()
		return 0, err
	}
	if r.offset >= r.object.Size() {
		return 0, io.EOF
	}
	if r.in == nil {
		r.chunkEnd = r.object.Size()
		if r.chunkSize > 0 && r.chunkSize < r.chunkEnd-r.offset {
			r.chunkEnd = r.offset + r.chunkSize
		}
		in, err := operations.Open(r.ctx, r.object, &fs.RangeOption{Start: r.offset, End: r.chunkEnd - 1})
		if err != nil {
			return 0, err
		}
		r.in = in
	}
	if int64(len(p)) > r.chunkEnd-r.offset {
		p = p[:r.chunkEnd-r.offset]
	}
	n, err := r.in.Read(p)
	r.offset += int64(n)
	if err != nil && r.ctx.Err() != nil {
		_ = r.Close()
		return n, r.ctx.Err()
	}
	if r.offset == r.chunkEnd {
		closeErr := r.Close()
		if err == io.EOF {
			err = nil
		}
		if err == nil {
			err = closeErr
		}
	} else if err == io.EOF {
		err = io.ErrUnexpectedEOF
	}
	return n, err
}

func (r *directReadSeeker) Seek(offset int64, whence int) (int64, error) {
	switch whence {
	case io.SeekStart:
	case io.SeekCurrent:
		offset += r.offset
	case io.SeekEnd:
		offset += r.object.Size()
	default:
		return r.offset, fmt.Errorf("invalid seek origin")
	}
	if offset < 0 {
		return r.offset, fmt.Errorf("negative seek position")
	}
	if offset != r.offset {
		if err := r.Close(); err != nil {
			return r.offset, err
		}
		r.offset = offset
	}
	return r.offset, nil
}

func (r *directReadSeeker) Close() error {
	if r.in == nil {
		return nil
	}
	in := r.in
	r.in = nil
	return in.Close()
}
