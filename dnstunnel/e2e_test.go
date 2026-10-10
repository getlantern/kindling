package dnstunnel

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These tests run the Go client against spark's real dns-tunnel-server, proving wire compatibility.
// Set DNSTUNNEL_SERVER_BIN to the server binary (cargo build --release -p dns-tunnel-server).

const testZone = "t.tunnel.test"

func startServer(t *testing.T) (resolver, pubkey string) {
	t.Helper()
	bin := os.Getenv("DNSTUNNEL_SERVER_BIN")
	if bin == "" {
		t.Skip("DNSTUNNEL_SERVER_BIN not set")
	}
	out, err := exec.Command(bin, "keygen").Output()
	require.NoError(t, err)
	var priv string
	for _, line := range strings.Split(string(out), "\n") {
		if f := strings.Fields(line); len(f) == 2 {
			switch f[0] {
			case "privkey":
				priv = f[1]
			case "pubkey":
				pubkey = f[1]
			}
		}
	}
	require.NotEmpty(t, priv)
	require.NotEmpty(t, pubkey)
	keyFile := filepath.Join(t.TempDir(), "privkey")
	require.NoError(t, os.WriteFile(keyFile, []byte(priv), 0o600))

	// Reserve a free UDP port, then hand it to the server.
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	require.NoError(t, err)
	resolver = pc.LocalAddr().String()
	pc.Close()

	cmd := exec.Command(bin, "serve", "--zone", testZone, "--privkey-file", keyFile, "--bind", resolver)
	cmd.Stderr = io.Discard
	require.NoError(t, cmd.Start())
	t.Cleanup(func() {
		cmd.Process.Kill()
		cmd.Wait()
	})
	time.Sleep(300 * time.Millisecond)
	return resolver, pubkey
}

func newTestClient(t *testing.T, resolver, pubkey string) *Client {
	t.Helper()
	c, err := New(Config{
		Zone:            testZone,
		ServerPublicKey: pubkey,
		Resolvers:       []string{resolver},
		Duplication:     1,
		IdleTimeout:     500 * time.Millisecond,
	})
	require.NoError(t, err)
	t.Cleanup(func() { c.Close() })
	return c
}

func TestE2E_HTTPThroughTunnel(t *testing.T) {
	resolver, pub := startServer(t)
	big := bytes.Repeat([]byte("0123456789abcdef"), 300<<10/16)
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/big":
			w.Write(big)
		case "/echo":
			// Read the whole body before responding: Go's HTTP/1 server may stop reading a request
			// body once the response starts, which a slow tunnel exposes.
			b, _ := io.ReadAll(r.Body)
			w.Write(b)
		default:
			fmt.Fprint(w, "hello through dns")
		}
	}))
	defer origin.Close()

	c := newTestClient(t, resolver, pub)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	rt, err := c.NewRoundTripper(ctx, "")
	require.NoError(t, err)
	hc := &http.Client{Transport: rt, Timeout: 60 * time.Second}

	t.Run("small GET", func(t *testing.T) {
		resp, err := hc.Get(origin.URL + "/")
		require.NoError(t, err)
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		assert.Equal(t, "hello through dns", string(body))
	})

	t.Run("POST echo", func(t *testing.T) {
		payload := bytes.Repeat([]byte{0xA5}, 6000)
		resp, err := hc.Post(origin.URL+"/echo", "application/octet-stream", bytes.NewReader(payload))
		require.NoError(t, err)
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		assert.Equal(t, payload, body)
	})

	t.Run("300KB download integrity", func(t *testing.T) {
		start := time.Now()
		resp, err := hc.Get(origin.URL + "/big")
		require.NoError(t, err)
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		assert.Equal(t, sha256.Sum256(big), sha256.Sum256(body))
		t.Logf("300KB in %v", time.Since(start))
	})

	t.Run("concurrent requests multiplex one session", func(t *testing.T) {
		var wg sync.WaitGroup
		errs := make(chan error, 4)
		for range 4 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				// A fresh transport per goroutine forces a separate stream each.
				tr := &http.Transport{DialContext: c.DialContext, DisableKeepAlives: true}
				resp, err := (&http.Client{Transport: tr, Timeout: 60 * time.Second}).Get(origin.URL + "/")
				if err != nil {
					errs <- err
					return
				}
				defer resp.Body.Close()
				b, _ := io.ReadAll(resp.Body)
				if string(b) != "hello through dns" {
					errs <- fmt.Errorf("bad body %q", b)
				}
			}()
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			assert.NoError(t, err)
		}
	})

	t.Run("domain target is resolved by the exit", func(t *testing.T) {
		port := origin.Listener.Addr().(*net.TCPAddr).Port
		resp, err := hc.Get(fmt.Sprintf("http://localhost:%d/", port))
		require.NoError(t, err)
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		assert.Equal(t, "hello through dns", string(body))
	})
}

func TestE2E_IdleTeardownAndRebuild(t *testing.T) {
	resolver, pub := startServer(t)
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "ok")
	}))
	defer origin.Close()
	c := newTestClient(t, resolver, pub)

	get := func() {
		tr := &http.Transport{DialContext: c.DialContext, DisableKeepAlives: true}
		resp, err := (&http.Client{Transport: tr, Timeout: 30 * time.Second}).Get(origin.URL)
		require.NoError(t, err)
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		require.Equal(t, "ok", string(b))
	}
	get()
	c.mu.Lock()
	first := c.pump
	c.mu.Unlock()
	require.NotNil(t, first)

	require.Eventually(t, first.isDone, 10*time.Second, 50*time.Millisecond, "idle session must tear down")

	get()
	c.mu.Lock()
	second := c.pump
	c.mu.Unlock()
	assert.NotSame(t, first, second, "a dial after teardown builds a fresh session")
}

func TestE2E_WrongServerKeyNeverEstablishes(t *testing.T) {
	resolver, _ := startServer(t)
	// A valid-looking key that isn't the server's: the SynAck signature must not verify.
	c := newTestClient(t, resolver, "Ty6wBHf3XGrUuC4+Q6mJd2TbeKpW4b4l2cVLmw9o4Yk=")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, err := c.NewRoundTripper(ctx, "")
	assert.Error(t, err)
}

// lossyRelay forwards UDP between one client and the server, dropping, duplicating and delaying
// packets in both directions, like a congested path through recursive resolvers.
func lossyRelay(t *testing.T, server string, loss, dup float64) string {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	require.NoError(t, err)
	up, err := net.Dial("udp", server)
	require.NoError(t, err)
	t.Cleanup(func() { pc.Close(); up.Close() })
	var mu sync.Mutex
	var client net.Addr
	r := rand.New(rand.NewPCG(3, 5))
	forward := func(write func([]byte)) func([]byte) {
		return func(b []byte) {
			mu.Lock()
			drop, copies, delay := r.Float64() < loss, 1, time.Duration(r.IntN(80))*time.Millisecond
			if r.Float64() < dup {
				copies = 2
			}
			mu.Unlock()
			if drop {
				return
			}
			msg := append([]byte(nil), b...)
			for range copies {
				time.AfterFunc(delay, func() { write(msg) })
			}
		}
	}
	toServer := forward(func(b []byte) { up.Write(b) })
	toClient := forward(func(b []byte) {
		mu.Lock()
		c := client
		mu.Unlock()
		if c != nil {
			pc.WriteTo(b, c)
		}
	})
	go func() {
		buf := make([]byte, 4096)
		for {
			n, from, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			mu.Lock()
			client = from
			mu.Unlock()
			toServer(buf[:n])
		}
	}()
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := up.Read(buf)
			if err != nil {
				return
			}
			toClient(buf[:n])
		}
	}()
	return pc.LocalAddr().String()
}

func TestE2E_LossyPathWithDeadResolverAndBoundedMemory(t *testing.T) {
	resolver, pub := startServer(t)
	payload := make([]byte, 200<<10)
	for i := range payload {
		payload[i] = byte(i * 7)
	}
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(payload)
	}))
	defer origin.Close()

	relay := lossyRelay(t, resolver, 0.15, 0.10)
	// A dead resolver first in the pool: duplication and failover must route around it.
	dead, err := net.ListenPacket("udp", "127.0.0.1:0")
	require.NoError(t, err)
	deadAddr := dead.LocalAddr().String()
	dead.Close()

	c, err := New(Config{
		Zone:            testZone,
		ServerPublicKey: pub,
		Resolvers:       []string{deadAddr, relay},
		Duplication:     2,
		IdleTimeout:     500 * time.Millisecond,
	})
	require.NoError(t, err)
	defer c.Close()

	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)
	var peak atomic.Uint64
	stop := make(chan struct{})
	go func() {
		var m runtime.MemStats
		for {
			select {
			case <-stop:
				return
			case <-time.After(10 * time.Millisecond):
				runtime.ReadMemStats(&m)
				if m.HeapInuse > peak.Load() {
					peak.Store(m.HeapInuse)
				}
			}
		}
	}()

	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	rt, err := c.NewRoundTripper(ctx, "")
	require.NoError(t, err)
	resp, err := (&http.Client{Transport: rt, Timeout: 2 * time.Minute}).Get(origin.URL)
	require.NoError(t, err)
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	close(stop)
	require.NoError(t, err)
	assert.Equal(t, sha256.Sum256(payload), sha256.Sum256(body))

	growth := int64(peak.Load()) - int64(before.HeapInuse)
	t.Logf("200KB over 15%% loss/10%% dup/0-80ms jitter + a dead resolver: %v, peak heap growth %d KB",
		time.Since(start).Round(time.Millisecond), growth/1024)
	// Generous ceiling (test harness, relay timers and the 200KB body itself count here too); the
	// point is catching an unbounded buffer, not benchmarking.
	assert.Less(t, growth, int64(8<<20))
}

func TestE2E_ConnLifecycle(t *testing.T) {
	resolver, pub := startServer(t)
	// An origin that accepts and then never reads, and one that streams forever.
	sink, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer sink.Close()
	go func() {
		for {
			c, err := sink.Accept()
			if err != nil {
				return
			}
			defer c.Close()
		}
	}()
	firehose, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer firehose.Close()
	go func() {
		for {
			c, err := firehose.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				buf := make([]byte, 4096)
				for {
					if _, err := c.Write(buf); err != nil {
						return
					}
				}
			}()
		}
	}()
	c := newTestClient(t, resolver, pub)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	t.Run("Close unblocks a backpressured Write", func(t *testing.T) {
		conn, err := c.DialContext(ctx, "tcp", sink.Addr().String())
		require.NoError(t, err)
		done := make(chan error, 1)
		go func() {
			_, err := conn.Write(make([]byte, 4<<20))
			done <- err
		}()
		time.Sleep(500 * time.Millisecond)
		conn.Close()
		select {
		case err := <-done:
			assert.Error(t, err)
		case <-time.After(3 * time.Second):
			t.Fatal("Write still blocked after Close")
		}
	})

	t.Run("expired write deadline fails without sending", func(t *testing.T) {
		conn, err := c.DialContext(ctx, "tcp", sink.Addr().String())
		require.NoError(t, err)
		defer conn.Close()
		conn.SetWriteDeadline(time.Now().Add(-time.Second))
		n, err := conn.Write([]byte("late"))
		assert.ErrorIs(t, err, os.ErrDeadlineExceeded)
		assert.Zero(t, n)
	})

	t.Run("a closed, unread stream does not pin the session", func(t *testing.T) {
		conn, err := c.DialContext(ctx, "tcp", firehose.Addr().String())
		require.NoError(t, err)
		time.Sleep(300 * time.Millisecond) // let downlink pile up unread
		c.mu.Lock()
		p := c.pump
		c.mu.Unlock()
		conn.Close()
		require.Eventually(t, p.isDone, time.Duration(closeLinger)*time.Millisecond+10*time.Second,
			100*time.Millisecond, "session must tear down once the closed stream lingers out")
	})
}
