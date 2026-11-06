package drive

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/lib/dircache"
	"github.com/rclone/rclone/lib/pacer"
	"github.com/rclone/rclone/vfs"
	"github.com/rclone/rclone/vfs/vfscommon"
	"github.com/stretchr/testify/require"
	"golang.org/x/oauth2"
	"google.golang.org/api/drive/v3"
	"google.golang.org/api/option"
)

// latencyFixture exercises the real Drive lookup and Open paths without credentials.
type latencyFixture struct {
	shortcuts                         int
	oauthDelay, listDelay, mediaDelay time.Duration
	timing                            bool
	oauth                             bool
	tokens                            atomic.Int64
	tokenNanos, listNanos, mediaNanos atomic.Int64
	directorySize                     int
	payload                           string
	unfiltered                        atomic.Int64
	server                            *httptest.Server
	lists, gets, media, bytes         atomic.Int64
	delay                             time.Duration
}

func newLatencyFixture(tb testing.TB, delay time.Duration) *latencyFixture {
	tb.Helper()
	x := &latencyFixture{delay: delay, payload: strings.Repeat("x", 8192)}
	x.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		defer func() {
			elapsed := time.Since(started).Nanoseconds()
			switch {
			case r.URL.Path == "/token":
				x.tokenNanos.Add(elapsed)
			case r.URL.Query().Get("alt") == "media":
				x.mediaNanos.Add(elapsed)
			case strings.HasSuffix(r.URL.Path, "/files"):
				x.listNanos.Add(elapsed)
			}
		}()
		delay := x.delay
		switch {
		case r.URL.Path == "/token" && x.oauthDelay > 0:
			delay = x.oauthDelay
		case r.URL.Query().Get("alt") == "media" && x.mediaDelay > 0:
			delay = x.mediaDelay
		case strings.HasSuffix(r.URL.Path, "/files") && x.listDelay > 0:
			delay = x.listDelay
		}
		select {
		case <-r.Context().Done():
			return
		case <-time.After(delay):
		}
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/token" {
			x.tokens.Add(1)
			_, _ = io.WriteString(w, `{"access_token":"synthetic-token","token_type":"Bearer","expires_in":3600}`)
			return
		}
		info := &drive.File{Id: "synthetic-file", Name: "media.bin", ModifiedTime: "2000-01-01T00:00:00Z", MimeType: "application/octet-stream", Size: 1 << 30, Md5Checksum: strings.Repeat("0", 32)}
		if r.URL.Query().Get("alt") == "media" {
			x.media.Add(1)
			var start, end int64
			if _, err := fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-%d", &start, &end); err != nil || start < 0 || end < start || end-start >= int64(len(x.payload)) {
				http.Error(w, "invalid synthetic range", http.StatusRequestedRangeNotSatisfiable)
				return
			}
			w.Header().Set("Content-Type", "application/octet-stream")
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, info.Size))
			w.WriteHeader(http.StatusPartialContent)
			n, _ := io.WriteString(w, x.payload[:int(end-start+1)])
			x.bytes.Add(int64(n))
		} else if strings.HasSuffix(r.URL.Path, "/files") {
			x.lists.Add(1)
			q := r.URL.Query().Get("q")
			if !strings.Contains(q, "name=") && x.directorySize > 0 {
				x.unfiltered.Add(1)
				first, _ := strconv.Atoi(r.URL.Query().Get("pageToken"))
				last := min(first+1000, x.directorySize)
				files := make([]*drive.File, 0, last-first)
				for i := first; i < last; i++ {
					item := *info
					if i != x.directorySize-1 {
						item.Id = fmt.Sprintf("synthetic-%d", i)
						item.Name = fmt.Sprintf("other-%d.bin", i)
					}
					if i < x.shortcuts {
						item.Md5Checksum = ""
						item.MimeType = shortcutMimeType
						item.ShortcutDetails = &drive.FileShortcutDetails{TargetId: "synthetic-file", TargetMimeType: "application/octet-stream"}
					}
					files = append(files, &item)
				}
				result := &drive.FileList{Files: files}
				if last < x.directorySize {
					result.NextPageToken = strconv.Itoa(last)
				}
				_ = json.NewEncoder(w).Encode(result)
				return
			}

			if !strings.Contains(q, "name='media.bin'") {
				// Each synthetic directory has the same name and a distinct parent ID.
				parent := strings.Split(strings.Split(q, " in parents")[0], "'")
				id := parent[len(parent)-2] + "-child"
				info = &drive.File{Id: id, Name: "dir", MimeType: driveFolderType}
			}
			_ = json.NewEncoder(w).Encode(&drive.FileList{Files: []*drive.File{info}})
		} else if strings.HasSuffix(r.URL.Path, "/files/synthetic-file") || strings.HasSuffix(r.URL.Path, "/files/root") {
			if strings.HasSuffix(r.URL.Path, "/files/root") {
				info.Id = "synthetic-root"
			}
			x.gets.Add(1)
			_ = json.NewEncoder(w).Encode(info)
		} else {
			http.NotFound(w, r)
		}
	}))
	tb.Cleanup(x.server.Close)
	return x
}

func (x *latencyFixture) newFs(tb testing.TB) *Fs {
	tb.Helper()
	ctx := context.Background()
	client := x.server.Client()
	if x.timing {
		client = &http.Client{Transport: &driveReadTimingTransport{base: client.Transport}}
	}
	if x.oauth {
		config := &oauth2.Config{ClientID: "synthetic-client", ClientSecret: "synthetic-secret", Endpoint: oauth2.Endpoint{TokenURL: x.server.URL + "/token"}}
		oauthContext := context.WithValue(ctx, oauth2.HTTPClient, client)
		client = oauth2.NewClient(oauthContext, config.TokenSource(oauthContext, &oauth2.Token{AccessToken: "expired-synthetic-token", RefreshToken: "synthetic-refresh", Expiry: time.Now().Add(-time.Hour)}))
	}
	service, err := drive.NewService(ctx, option.WithHTTPClient(client), option.WithEndpoint(x.server.URL+"/"))
	require.NoError(tb, err)
	f := &Fs{svc: service, client: client, rootFolderID: "synthetic-root", pacer: fs.NewPacer(ctx, pacer.NewGoogleDrive(pacer.MinSleep(0), pacer.Burst(1000000)))}
	f.opt.ReadTiming = x.timing
	f.features = &fs.Features{}
	f.ci = fs.GetConfig(ctx)
	f.dirResourceKeys = new(sync.Map)
	f.opt.SkipGdocs = true
	f.opt.V2DownloadMinSize = -1
	f.dirCache = dircache.New("", f.rootFolderID, f)
	return f
}

func latencyRead(tb testing.TB, ctx context.Context, o fs.Object, offset int64) time.Duration {
	tb.Helper()
	start := time.Now()
	in, err := o.Open(ctx, &fs.RangeOption{Start: offset, End: offset + 8191})
	require.NoError(tb, err)
	var first [1]byte
	_, err = io.ReadFull(in, first[:])
	require.NoError(tb, err)
	ttfb := time.Since(start)
	require.Equal(tb, byte('x'), first[0])
	n, err := io.Copy(io.Discard, in)
	require.NoError(tb, err)
	require.Equal(tb, int64(8191), n)
	require.NoError(tb, in.Close())
	return ttfb
}

func TestDriveReadLatencyBaseline(t *testing.T) {
	ctx := context.Background()
	x := newLatencyFixture(t, 0)
	f := x.newFs(t)
	o, err := f.NewObject(ctx, "dir/dir/media.bin")
	require.NoError(t, err)
	require.Equal(t, int64(3), x.lists.Load())
	latencyRead(t, ctx, o, 0)
	latencyRead(t, ctx, o, 1<<20)
	require.Equal(t, int64(3), x.lists.Load(), "reopening an object must not resolve its path")
	require.Equal(t, int64(0), x.gets.Load())
	require.Equal(t, int64(2), x.media.Load())
	require.Equal(t, int64(16384), x.bytes.Load())
	_, err = f.NewObject(ctx, "dir/dir/media.bin")
	require.NoError(t, err)
	require.Equal(t, int64(4), x.lists.Load(), "warm directory cache still resolves file metadata")
	info, err := f.getFile(ctx, "synthetic-file", f.getFileFields(ctx))
	require.NoError(t, err)
	byID, err := f.newObjectWithInfo(ctx, info.Name, info)
	require.NoError(t, err)
	latencyRead(t, ctx, byID, 8192)
	require.Equal(t, int64(1), x.gets.Load())
	require.Equal(t, int64(4), x.lists.Load())
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	_, err = o.Open(canceled, &fs.RangeOption{Start: 0, End: 8191})
	require.ErrorIs(t, err, context.Canceled)
}

func BenchmarkDriveReadLatency(b *testing.B) {
	for _, delay := range []time.Duration{0, 2 * time.Millisecond} {
		for _, depth := range []int{0, 4, 16} {
			for _, mode := range []string{"cold-path", "warm-path", "id-metadata", "reuse-object"} {
				b.Run(fmt.Sprintf("delay=%s/depth=%d/%s", delay, depth, mode), func(b *testing.B) {
					ctx := context.Background()
					x := newLatencyFixture(b, delay)
					f := x.newFs(b)
					remote := strings.Repeat("dir/", depth) + "media.bin"
					o, err := f.NewObject(ctx, remote)
					require.NoError(b, err)
					l0, g0, m0, n0 := x.lists.Load(), x.gets.Load(), x.media.Load(), x.bytes.Load()
					var openTTFB, lookupTTFB time.Duration
					b.ResetTimer()
					for i := 0; i < b.N; i++ {
						lookupStart := time.Now()
						switch mode {
						case "cold-path":
							f = x.newFs(b)
							o, err = f.NewObject(ctx, remote)
						case "warm-path":
							o, err = f.NewObject(ctx, remote)
						case "id-metadata":
							var info *drive.File
							info, err = f.getFile(ctx, "synthetic-file", f.getFileFields(ctx))
							if err == nil {
								o, err = f.newObjectWithInfo(ctx, info.Name, info)
							}
						}
						require.NoError(b, err)
						lookupTime := time.Since(lookupStart)
						firstByte := latencyRead(b, ctx, o, int64(i%2)<<20)
						openTTFB += firstByte
						lookupTTFB += lookupTime + firstByte
					}
					b.StopTimer()
					b.ReportMetric(float64(x.lists.Load()-l0)/float64(b.N), "list/op")
					b.ReportMetric(float64(x.gets.Load()-g0)/float64(b.N), "metadata/op")
					b.ReportMetric(float64(x.media.Load()-m0)/float64(b.N), "media/op")
					b.ReportMetric(float64(x.bytes.Load()-n0)/float64(b.N), "media-B/op")
					b.ReportMetric(float64(openTTFB.Nanoseconds())/float64(b.N), "open-first-byte-ns/op")
					b.ReportMetric(float64(lookupTTFB.Nanoseconds())/float64(b.N), "lookup-first-byte-ns/op")
					b.SetBytes(8192)
				})
			}
		}
	}
}

// TestDriveVFSFirstRange exercises the lookup and ServeContent path used by serve http.
func TestDriveVFSFirstRange(t *testing.T) {
	for _, size := range []int{1, 1001, 10001} {
		t.Run(strconv.Itoa(size), func(t *testing.T) {
			ctx, ci := fs.AddConfig(context.Background())
			ci.BufferSize = 0
			x := newLatencyFixture(t, 2*time.Millisecond)
			x.oauth = true
			x.directorySize = size
			x.payload = strings.Repeat("x", 8<<20)
			f := x.newFs(t)
			opt := vfscommon.Opt
			opt.CacheMode = vfscommon.CacheModeOff
			opt.DirCacheTime = fs.Duration(5 * time.Minute)
			opt.PollInterval = 0
			opt.ReadOnly = true
			opt.ChunkSize = 8 << 20
			opt.ChunkSizeLimit = 8 << 20
			filesystem := vfs.New(ctx, f, &opt)
			defer filesystem.Shutdown()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				node, err := filesystem.Stat("media.bin")
				if err != nil {
					http.Error(w, err.Error(), 500)
					return
				}
				in, err := node.(*vfs.File).Open(os.O_RDONLY)
				if err != nil {
					http.Error(w, err.Error(), 500)
					return
				}
				defer in.Close()
				http.ServeContent(w, r, "media.bin", node.ModTime(), in)
			}))
			defer server.Close()
			var hash [32]byte
			for i, offset := range []int64{0, 8 << 20, 0} {
				start := time.Now()
				req, err := http.NewRequestWithContext(ctx, "GET", server.URL+"/media.bin", nil)
				require.NoError(t, err)
				req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", offset, offset+(8<<20)-1))
				response, err := server.Client().Do(req)
				require.NoError(t, err)
				headers := time.Since(start)
				require.Equal(t, http.StatusPartialContent, response.StatusCode)
				h := sha256.New()
				n, err := io.Copy(h, response.Body)
				require.NoError(t, err)
				require.NoError(t, response.Body.Close())
				require.Equal(t, int64(8<<20), n)
				if i == 0 {
					copy(hash[:], h.Sum(nil))
				} else {
					require.Equal(t, hash[:], h.Sum(nil))
				}
				require.Equal(t, int64((size+999)/1000), x.unfiltered.Load())
				require.Equal(t, int64(0), x.gets.Load())
				require.Equal(t, int64(1), x.tokens.Load())
				t.Logf("directory=%d range=%d headers=%s total=%s list=%d metadata=%d media=%d bytes=%d tokens=%d token-time=%s list-time=%s media-time=%s", size, i, headers, time.Since(start), x.lists.Load(), x.gets.Load(), x.media.Load(), x.bytes.Load(), x.tokens.Load(), time.Duration(x.tokenNanos.Load()), time.Duration(x.listNanos.Load()), time.Duration(x.mediaNanos.Load()))
			}
			// The existing direct lookup helper sends a name-filtered query instead of listing the directory.
			before := x.unfiltered.Load()
			o, err := f.NewObject(ctx, "media.bin")
			require.NoError(t, err)
			require.Equal(t, before, x.unfiltered.Load())
			require.Equal(t, before+1, x.lists.Load())

			info, err := f.getFile(ctx, "synthetic-file", f.getFileFields(ctx))
			require.NoError(t, err)
			byID, err := f.newObjectWithInfo(ctx, info.Name, info)
			require.NoError(t, err)
			for _, object := range []fs.Object{o, byID} {
				in, err := object.Open(ctx, &fs.RangeOption{Start: 0, End: (8 << 20) - 1})
				require.NoError(t, err)
				n, err := io.Copy(io.Discard, in)
				require.NoError(t, err)
				require.NoError(t, in.Close())
				require.Equal(t, int64(8<<20), n)
			}
			require.Equal(t, int64(1), x.gets.Load())
			require.Equal(t, before+1, x.lists.Load())

		})
	}
}

func TestDriveRootIDBaseline(t *testing.T) {
	x := newLatencyFixture(t, 2*time.Millisecond)
	x.oauth = true
	f := x.newFs(t)
	started := time.Now()
	id, err := f.getRootID(context.Background())
	require.NoError(t, err)
	require.Equal(t, "synthetic-root", id)
	require.Equal(t, int64(1), x.tokens.Load())
	require.Equal(t, int64(1), x.gets.Load())
	require.Equal(t, int64(0), x.lists.Load())
	t.Logf("root lookup including synthetic OAuth refresh: %s; token handler: %s", time.Since(started), time.Duration(x.tokenNanos.Load()))
}

func TestDriveColdReadAttribution(t *testing.T) {
	for _, phase := range []string{"oauth", "listing", "media"} {
		t.Run(phase, func(t *testing.T) {
			ctx, ci := fs.AddConfig(context.Background())
			ci.BufferSize = 0
			x := newLatencyFixture(t, 0)
			x.oauth = true
			x.timing = true
			x.directorySize = 1001
			switch phase {
			case "oauth":
				x.oauthDelay = 120 * time.Millisecond
			case "listing":
				x.listDelay = 60 * time.Millisecond
			case "media":
				x.mediaDelay = 120 * time.Millisecond
			}
			f := x.newFs(t)
			opt := vfscommon.Opt
			opt.ReadOnly = true
			opt.CacheMode = vfscommon.CacheModeOff
			opt.PollInterval = 0
			opt.DirCacheTime = fs.Duration(5 * time.Minute)
			opt.ChunkSize = 8192
			opt.ChunkSizeLimit = 8192
			filesystem := vfs.New(ctx, f, &opt)
			defer filesystem.Shutdown()
			for i := 0; i < 2; i++ {
				start := time.Now()
				node, err := filesystem.Stat("media.bin")
				require.NoError(t, err)
				stat := time.Since(start)
				in, err := node.(*vfs.File).Open(os.O_RDONLY)
				require.NoError(t, err)
				_, err = in.Seek(int64(i*8192), io.SeekStart)
				require.NoError(t, err)
				var first [1]byte
				_, err = io.ReadFull(in, first[:])
				require.NoError(t, err)
				firstByte := time.Since(start)
				_, err = io.CopyN(io.Discard, in, 8191)
				require.NoError(t, err)
				require.NoError(t, in.Close())
				require.Equal(t, int64(1), x.tokens.Load())
				require.Equal(t, int64(2), x.unfiltered.Load())
				t.Logf("injected=%s read=%d stat=%s first-byte=%s total=%s", phase, i, stat, firstByte, time.Since(start))
			}
		})
	}
}

func TestDriveColdShortcutAmplification(t *testing.T) {
	ctx, ci := fs.AddConfig(context.Background())
	ci.BufferSize = 0
	x := newLatencyFixture(t, 5*time.Millisecond)
	x.directorySize = 13
	x.shortcuts = 12
	f := x.newFs(t)
	opt := vfscommon.Opt
	opt.ReadOnly = true
	opt.CacheMode = vfscommon.CacheModeOff
	opt.PollInterval = 0
	opt.DirCacheTime = fs.Duration(5 * time.Minute)
	filesystem := vfs.New(ctx, f, &opt)
	defer filesystem.Shutdown()
	for i := 0; i < 2; i++ {
		start := time.Now()
		node, err := filesystem.Stat("media.bin")
		require.NoError(t, err)
		require.True(t, node.IsFile())
		require.Equal(t, int64(1), x.lists.Load())
		require.Equal(t, int64(12), x.gets.Load())
		t.Logf("read=%d stat=%s list=%d shortcut-metadata=%d media=%d", i, time.Since(start), x.lists.Load(), x.gets.Load(), x.media.Load())
	}
	before := x.gets.Load()
	_, err := f.NewObject(ctx, "media.bin")
	require.NoError(t, err)
	require.Equal(t, before, x.gets.Load(), "a direct regular-file lookup does not resolve unrelated shortcuts")
}
