package http

import (
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rclone/rclone/cmd/serve/proxy"
	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/filter"
	"github.com/rclone/rclone/fstest/mockobject"
	"github.com/rclone/rclone/vfs/vfscommon"
	"github.com/stretchr/testify/require"
)

type directCountingFs struct {
	fs.Fs
	lists, lookups atomic.Int64
}

func (f *directCountingFs) List(ctx context.Context, remote string) (fs.DirEntries, error) {
	f.lists.Add(1)
	return f.Fs.List(ctx, remote)
}

func (f *directCountingFs) NewObject(ctx context.Context, remote string) (fs.Object, error) {
	f.lookups.Add(1)
	return f.Fs.NewObject(ctx, remote)
}

func TestDirectReadHTTP(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	file := filepath.Join(root, "media.bin")
	require.NoError(t, os.WriteFile(file, []byte("0123456789abcdef"), 0600))
	modified := time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
	require.NoError(t, os.Chtimes(file, modified, modified))
	backend, err := fs.NewFs(ctx, ":local:"+root)
	require.NoError(t, err)
	f := &directCountingFs{Fs: backend}
	opt := DefaultOpt
	opt.DirectRead = true
	opt.HTTP.ListenAddr = []string{"127.0.0.1:0"}
	opt.Auth.BasicUser = "synthetic-user"
	opt.Auth.BasicPass = "synthetic-pass"
	vfsOpt := vfscommon.Opt
	vfsOpt.ReadOnly = true
	vfsOpt.CacheMode = vfscommon.CacheModeOff
	vfsOpt.PollInterval = 0
	vfsOpt.ChunkSize = 4
	vfsOpt.CaseInsensitive = false
	s, err := newServer(ctx, f, &opt, &vfsOpt, &proxy.Options{})
	require.NoError(t, err)
	done := make(chan error, 1)
	go func() { done <- s.Serve() }()
	defer func() { require.NoError(t, s.Shutdown()); require.NoError(t, <-done) }()
	url := s.server.URLs()[0]
	response, err := http.Get(url + "media.bin")
	require.NoError(t, err)
	require.Equal(t, 401, response.StatusCode)
	require.NoError(t, response.Body.Close())
	require.Zero(t, f.lookups.Load())
	request := func(method, path, rangeValue, ifModified string) (int, http.Header, string) {
		req, err := http.NewRequest(method, url+path, nil)
		require.NoError(t, err)
		req.SetBasicAuth("synthetic-user", "synthetic-pass")
		if rangeValue != "" {
			req.Header.Set("Range", rangeValue)
		}
		if ifModified != "" {
			req.Header.Set("If-Modified-Since", ifModified)
		}
		res, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		data, err := io.ReadAll(res.Body)
		require.NoError(t, err)
		require.NoError(t, res.Body.Close())
		return res.StatusCode, res.Header, string(data)
	}
	code, headers, data := request("HEAD", "media.bin", "", "")
	require.Equal(t, 200, code)
	require.Equal(t, "16", headers.Get("Content-Length"))
	require.Empty(t, data)
	code, headers, data = request("GET", "media.bin", "bytes=5-10", "")
	require.Equal(t, 206, code)
	require.Equal(t, "bytes 5-10/16", headers.Get("Content-Range"))
	require.Equal(t, "56789a", data)
	code, _, data = request("GET", "media.bin", "bytes=-3", "")
	require.Equal(t, 206, code)
	require.Equal(t, "def", data)
	code, _, _ = request("GET", "media.bin", "bytes=16-", "")
	require.Equal(t, 416, code)
	code, _, data = request("GET", "media.bin", "", modified.Format(http.TimeFormat))
	require.Equal(t, 304, code)
	require.Empty(t, data)
	code, headers, data = request("GET", "media.bin", "bytes=1-2,10-11", "")
	require.Equal(t, 206, code)
	require.Contains(t, headers.Get("Content-Type"), "multipart/byteranges")
	require.Contains(t, data, "12")
	require.Contains(t, data, "ab")
	before := f.lookups.Load()
	code, _, _ = request("GET", "%2e%2e/media.bin", "", "")
	require.Equal(t, 404, code)
	require.Equal(t, before, f.lookups.Load())
	code, _, _ = request("GET", "dir%5cmedia.bin", "", "")
	require.Equal(t, 404, code)
	require.Equal(t, before, f.lookups.Load())
	require.NoError(t, os.Rename(file, filepath.Join(root, "moved.bin")))
	code, _, _ = request("HEAD", "media.bin", "", "")
	require.Equal(t, 404, code)
	code, _, _ = request("HEAD", "moved.bin", "", "")
	require.Equal(t, 200, code)
	require.NoError(t, os.Remove(filepath.Join(root, "moved.bin")))
	code, _, _ = request("HEAD", "moved.bin", "", "")
	require.Equal(t, 404, code)
	require.Zero(t, f.lists.Load(), "known file requests must not enumerate siblings")
}

func TestDirectReadSeeker(t *testing.T) {
	object := mockobject.New("synthetic.bin").WithContent([]byte("0123456789abcdef"), mockobject.SeekModeNone)
	reader := &directReadSeeker{ctx: context.Background(), object: object, chunkSize: 4}
	_, err := reader.Seek(5, io.SeekStart)
	require.NoError(t, err)
	data, err := io.ReadAll(reader)
	require.NoError(t, err)
	require.Equal(t, "56789abcdef", string(data))
	_, err = reader.Seek(-3, io.SeekEnd)
	require.NoError(t, err)
	data, err = io.ReadAll(reader)
	require.NoError(t, err)
	require.Equal(t, "def", string(data))
	_, err = reader.Seek(-1, io.SeekStart)
	require.Error(t, err)
	require.NoError(t, reader.Close())
}

func TestDirectReadGuards(t *testing.T) {
	for _, option := range []string{"writable", "cache", "links", "metadata", "case", "duplicates", "no-seek", "no-modtime", "filter", "proxy"} {
		t.Run(option, func(t *testing.T) {
			ctx := context.Background()
			opt := DefaultOpt
			opt.DirectRead = true
			vfsOpt := vfscommon.Opt
			vfsOpt.ReadOnly = true
			vfsOpt.CacheMode = vfscommon.CacheModeOff
			vfsOpt.CaseInsensitive = false
			proxyOpt := proxy.Options{}
			switch option {
			case "writable":
				vfsOpt.ReadOnly = false
			case "cache":
				vfsOpt.CacheMode = vfscommon.CacheModeFull
			case "links":
				vfsOpt.Links = true
			case "metadata":
				vfsOpt.MetadataExtension = ".metadata"
			case "case":
				vfsOpt.CaseInsensitive = true
			case "duplicates":
				vfsOpt.BlockNormDupes = true
			case "no-seek":
				vfsOpt.NoSeek = true
			case "no-modtime":
				vfsOpt.NoModTime = true
			case "proxy":
				proxyOpt.AuthProxy = "synthetic-proxy"
			case "filter":
				active, err := filter.NewFilter(nil)
				require.NoError(t, err)
				require.NoError(t, active.Add(false, "*.private"))
				ctx = filter.ReplaceConfig(ctx, active)
			}
			_, err := newServer(ctx, nil, &opt, &vfsOpt, &proxyOpt)
			require.Error(t, err)
			require.True(t, strings.Contains(err.Error(), "--direct-read"))
		})
	}
}

type directRangeObject struct {
	fs.Object
	ranges []string
}

func (o *directRangeObject) Open(ctx context.Context, options ...fs.OpenOption) (io.ReadCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	for _, option := range options {
		if r, ok := option.(*fs.RangeOption); ok {
			_, value := r.Header()
			o.ranges = append(o.ranges, value)
		}
	}
	return o.Object.Open(ctx, options...)
}

func TestDirectReadBoundedRangesAndCancellation(t *testing.T) {
	content := mockobject.New("synthetic.bin").WithContent([]byte("0123456789abcdef"), mockobject.SeekModeNone)
	object := &directRangeObject{Object: content}
	ctx, cancel := context.WithCancel(context.Background())
	reader := &directReadSeeker{ctx: ctx, object: object, chunkSize: 4}
	_, err := reader.Seek(5, io.SeekStart)
	require.NoError(t, err)
	data, err := io.ReadAll(reader)
	require.NoError(t, err)
	require.Equal(t, "56789abcdef", string(data))
	require.Equal(t, []string{"bytes=5-8", "bytes=9-12", "bytes=13-15"}, object.ranges)
	_, err = reader.Seek(0, io.SeekStart)
	require.NoError(t, err)
	cancel()
	_, err = reader.Read(make([]byte, 1))
	require.ErrorIs(t, err, context.Canceled)
	require.NoError(t, reader.Close())
}

type directCancelObject struct {
	fs.Object
	opened chan struct{}
	closed chan struct{}
}

func (o *directCancelObject) Open(ctx context.Context, _ ...fs.OpenOption) (io.ReadCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	close(o.opened)
	return &directCancelBody{ctx: ctx, closed: o.closed}, nil
}

type directCancelBody struct {
	ctx    context.Context
	closed chan struct{}
	once   sync.Once
}

func (b *directCancelBody) Read([]byte) (int, error) { <-b.ctx.Done(); return 0, b.ctx.Err() }
func (b *directCancelBody) Close() error             { b.once.Do(func() { close(b.closed) }); return nil }

func TestDirectReadInFlightCancellation(t *testing.T) {
	object := &directCancelObject{Object: mockobject.New("cancel.bin").WithContent([]byte("0123456789abcdef"), mockobject.SeekModeNone), opened: make(chan struct{}), closed: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	reader := &directReadSeeker{ctx: ctx, object: object, chunkSize: 4}
	done := make(chan error, 1)
	go func() { _, err := reader.Read(make([]byte, 1)); done <- err }()
	select {
	case <-object.opened:
	case <-time.After(time.Second):
		t.Fatal("backend did not open")
	}
	cancel()
	select {
	case err := <-done:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(time.Second):
		t.Fatal("read did not stop on cancellation")
	}
	require.NoError(t, reader.Close())
	select {
	case <-object.closed:
	case <-time.After(time.Second):
		t.Fatal("backend body was not closed")
	}
}
