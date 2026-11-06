package drive

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rclone/rclone/lib/pacer"
	"github.com/stretchr/testify/require"
)

func TestDriveReadTimingTransport(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Private-Response", "private-header")
		_, _ = io.WriteString(w, "private-body")
	}))
	defer server.Close()
	var mu sync.Mutex
	var lines []string
	transport := &driveReadTimingTransport{base: server.Client().Transport, report: func(format string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		lines = append(lines, fmt.Sprintf(format, args...))
	}}
	client := &http.Client{Transport: transport}
	for _, suffix := range []string{"/token", "/files?q=name%3D%27private-name%27", "/files/private-id?alt=media&key=private-key", "/files/root"} {
		req, err := http.NewRequest("GET", server.URL+suffix, nil)
		require.NoError(t, err)
		req.Header.Set("Authorization", "Bearer private-token")
		res, err := client.Do(req)
		require.NoError(t, err)
		body, err := io.ReadAll(res.Body)
		require.NoError(t, err)
		require.Equal(t, "private-body", string(body))
		require.NoError(t, res.Body.Close())
	}
	transport.CloseIdleConnections()
	mu.Lock()
	defer mu.Unlock()
	output := strings.Join(lines, "\n")
	for _, secret := range []string{"private-name", "private-id", "private-key", "private-token", "private-header", "private-body", server.URL, "Authorization"} {
		require.NotContains(t, output, secret)
	}
	for _, marker := range []string{"operation=oauth", "operation=list-filtered", "operation=media", "operation=root-metadata", "event=tls-finished", "reused=true", "event=first-response-byte", "event=http-headers", "event=body-read-finished", "event=body-first-read", "bytes=12"} {
		require.Contains(t, output, marker)
	}
}

type timingFailTransport struct{}

func (timingFailTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("private transport error with private-id")
}

func TestDriveReadTimingTransportError(t *testing.T) {
	var output string
	transport := &driveReadTimingTransport{base: timingFailTransport{}, report: func(format string, args ...any) { output += fmt.Sprintf(format, args...) }}
	req, err := http.NewRequestWithContext(context.Background(), "GET", "https://synthetic.invalid/files/private-id", nil)
	require.NoError(t, err)
	response, err := transport.RoundTrip(req)
	require.Error(t, err)
	require.Nil(t, response)
	require.Contains(t, output, "status=0 failed=true")
	require.NotContains(t, output, "private-id")
	require.NotContains(t, output, "private transport error")
}

func TestDriveReadTimingCancellation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer server.Close()
	var mu sync.Mutex
	var output string
	transport := &driveReadTimingTransport{base: server.Client().Transport, report: func(format string, args ...any) { mu.Lock(); defer mu.Unlock(); output += fmt.Sprintf(format, args...) }}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", server.URL+"/files/private-id?alt=media", nil)
	require.NoError(t, err)
	response, err := (&http.Client{Transport: transport}).Do(req)
	require.NoError(t, err)
	cancel()
	_, err = io.ReadAll(response.Body)
	require.ErrorIs(t, err, context.Canceled)
	require.NoError(t, response.Body.Close())
	mu.Lock()
	defer mu.Unlock()
	require.Contains(t, output, "event=body-read-finished")
	require.Contains(t, output, "failed=true")
	require.NotContains(t, output, "private-id")
}

func TestDriveReadTimingPacing(t *testing.T) {
	x := newLatencyFixture(t, 0)
	f := x.newFs(t)
	f.opt.ReadTiming = true
	f.pacer.SetCalculator(pacer.NewDefault(pacer.MinSleep(25*time.Millisecond), pacer.MaxSleep(25*time.Millisecond)))
	require.NoError(t, f.pacer.Call(func() (bool, error) { return false, nil }))
	calls := 0
	started := time.Now()
	err := f.readTimingCall("metadata", func() (bool, error) {
		calls++
		if calls == 1 {
			return true, errors.New("synthetic retry")
		}
		return false, nil
	})
	require.NoError(t, err)
	require.Equal(t, 2, calls)
	require.GreaterOrEqual(t, time.Since(started), 20*time.Millisecond)
}
