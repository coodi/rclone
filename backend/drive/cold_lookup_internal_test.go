package drive

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	_ "github.com/rclone/rclone/cmd/serve/http"
	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/cache"
	"github.com/rclone/rclone/fs/rc"
	"github.com/rclone/rclone/fs/rc/jobs"
	"github.com/rclone/rclone/fs/rc/rcserver"
	"github.com/rclone/rclone/vfs"
	"github.com/rclone/rclone/vfs/vfscommon"
	"github.com/stretchr/testify/require"
)

// TestDriveColdKnownPath compares cold VFS lookup with the existing RC file route.
func TestDriveColdKnownPath(t *testing.T) {
	ctx, ci := fs.AddConfig(context.Background())
	ci.BufferSize = 0
	t.Run("vfs-33-pages", func(t *testing.T) {
		x := newLatencyFixture(t, 0)
		x.directorySize = 32001
		f := x.newFs(t)
		opt := vfscommon.Opt
		opt.ReadOnly = true
		opt.CacheMode = vfscommon.CacheModeOff
		opt.DirCacheTime = fs.Duration(5 * time.Minute)
		opt.PollInterval = 0
		filesystem := vfs.New(ctx, f, &opt)
		defer filesystem.Shutdown()
		started := time.Now()
		node, err := filesystem.Stat("media.bin")
		require.NoError(t, err)
		require.True(t, node.IsFile())
		require.Equal(t, int64(33), x.unfiltered.Load())
		require.Equal(t, int64(0), x.gets.Load())
		require.Equal(t, int64(0), x.media.Load())
		t.Logf("cold lookup: %s, unfiltered=%d metadata=%d media=%d", time.Since(started), x.unfiltered.Load(), x.gets.Load(), x.media.Load())
	})
	t.Run("existing-rc-file-route", func(t *testing.T) {
		x := newLatencyFixture(t, 0)
		x.directorySize = 32001
		x.payload = strings.Repeat("x", 8<<20)
		f := x.newFs(t)
		f.name = "synthetic-cold-probe"
		cache.Put("synthetic-cold-probe:", f)
		defer cache.ClearConfig(f.name)
		opt := rc.Opt
		opt.Enabled = true
		opt.Serve = true
		opt.Files = ""
		opt.HTTP.ListenAddr = []string{"127.0.0.1:0"}
		opt.Auth.BasicUser = "synthetic-user"
		opt.Auth.BasicPass = "synthetic-pass"
		server, err := rcserver.Start(ctx, &opt)
		defer jobs.SetOpt(&rc.Opt)
		require.NoError(t, err)
		defer func() { require.NoError(t, server.Shutdown()); server.Wait() }()
		url := server.URLs()[0] + "[synthetic-cold-probe:]/media.bin"
		response, err := http.Get(url)
		require.NoError(t, err)
		require.Equal(t, http.StatusUnauthorized, response.StatusCode)
		require.NoError(t, response.Body.Close())
		require.Equal(t, int64(0), x.lists.Load(), "unauthenticated requests must not reach the backend")
		for _, method := range []string{"HEAD", "GET"} {
			started := time.Now()
			request, err := http.NewRequestWithContext(ctx, method, url, nil)
			require.NoError(t, err)
			request.SetBasicAuth("synthetic-user", "synthetic-pass")
			if method == "GET" {
				request.Header.Set("Range", "bytes=8388608-16777215")
			}
			response, err = http.DefaultClient.Do(request)
			require.NoError(t, err)
			headers := time.Since(started)
			n, err := io.Copy(io.Discard, response.Body)
			require.NoError(t, err)
			require.NoError(t, response.Body.Close())
			if method == "HEAD" {
				require.Equal(t, http.StatusOK, response.StatusCode)
				require.Zero(t, n)
				require.Equal(t, int64(0), x.media.Load())
			} else {
				require.Equal(t, http.StatusPartialContent, response.StatusCode)
				require.Equal(t, int64(8<<20), n)
				require.Equal(t, "bytes 8388608-16777215/1073741824", response.Header.Get("Content-Range"))
				require.Equal(t, int64(1), x.media.Load())
			}
			require.Equal(t, int64(0), x.unfiltered.Load())
			t.Logf("%s headers=%s total=%s filtered-lists=%d metadata=%d media=%d", method, headers, time.Since(started), x.lists.Load(), x.gets.Load(), x.media.Load())
		}
		require.Equal(t, int64(2), x.lists.Load())
		require.Equal(t, int64(2), x.gets.Load(), "the existing RC helper additionally reads object metadata per request")
	})
}

func TestDriveHTTPDirectRead(t *testing.T) {
	for _, direct := range []bool{false, true} {
		t.Run(fmt.Sprint(direct), func(t *testing.T) {
			ctx, ci := fs.AddConfig(context.Background())
			ci.BufferSize = 0
			x := newLatencyFixture(t, 0)
			x.directorySize = 32001
			x.payload = strings.Repeat("x", 8<<20)
			x.resourceKey = "synthetic-resource-key"
			x.sharedDrive = true
			f := x.newFs(t)
			f.name = fmt.Sprintf("synthetic-http-direct-%v", direct)
			cache.Put(f.name+":", f)
			defer cache.ClearConfig(f.name)
			start := rc.Calls.Get("serve/start")
			require.NotNil(t, start)
			out, err := start.Fn(ctx, rc.Params{"type": "http", "fs": f.name + ":", "addr": "127.0.0.1:0", "read_only": true, "vfs_cache_mode": "off", "vfs_read_chunk_size": "8Mi", "poll_interval": "0", "direct_read": direct, "user": "synthetic-user", "pass": "synthetic-pass"})
			require.NoError(t, err)
			defer func() {
				_, err := rc.Calls.Get("serve/stop").Fn(ctx, rc.Params{"id": out["id"]})
				require.NoError(t, err)
			}()
			url := "http://" + out["addr"].(string) + "/media.bin"
			for _, method := range []string{"HEAD", "GET", "GET"} {
				started := time.Now()
				request, err := http.NewRequestWithContext(ctx, method, url, nil)
				require.NoError(t, err)
				request.SetBasicAuth("synthetic-user", "synthetic-pass")
				if method == "GET" {
					request.Header.Set("Range", "bytes=8388608-16777215")
				}
				response, err := http.DefaultClient.Do(request)
				require.NoError(t, err)
				headers := time.Since(started)
				n, err := io.Copy(io.Discard, response.Body)
				require.NoError(t, err)
				require.NoError(t, response.Body.Close())
				if method == "HEAD" {
					require.Equal(t, 200, response.StatusCode)
					require.Zero(t, n)
					require.Equal(t, int64(0), x.media.Load())
				} else {
					require.Equal(t, 206, response.StatusCode)
					require.Equal(t, int64(8<<20), n)
				}
				if direct {
					require.Zero(t, x.unfiltered.Load())
				} else {
					require.Equal(t, int64(33), x.unfiltered.Load())
				}
				require.Zero(t, x.gets.Load(), "the HTTP file path must not need separate object metadata")
				t.Logf("direct=%v %s headers=%s total=%s lists=%d media=%d", direct, method, headers, time.Since(started), x.lists.Load(), x.media.Load())
			}
			if direct {
				require.Equal(t, int64(3), x.lists.Load())
			}
		})
	}
}

func TestDriveDirectLookupOwnerFilter(t *testing.T) {
	for _, owned := range []bool{false, true} {
		t.Run(fmt.Sprint(owned), func(t *testing.T) {
			x := newLatencyFixture(t, 0)
			x.owned = owned
			f := x.newFs(t)
			f.opt.AuthOwnerOnly = true
			_, err := f.NewObject(context.Background(), "media.bin")
			if owned {
				require.NoError(t, err)
			} else {
				require.ErrorIs(t, err, fs.ErrorObjectNotFound, "direct lookup must honor the same owner restriction as directory listing")
			}
		})
	}
}
