package main

import (
	"crypto/tls"
	"crypto/x509"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/mtl802/meshconsole/internal/pki"
	"golang.org/x/net/netutil"
)

// acceptingLoop 在后台循环 Accept，把接受的连接送入 ch（调用方持住以占用配额）。
func acceptingLoop(ln net.Listener, ch chan<- net.Conn, errCh chan<- error) {
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				errCh <- err
				return
			}
			ch <- c
		}
	}()
}

// TestLimitListenerQueues 限额生效语义（SPEC-M1b-a 验收 4）：
// 配额满时第 N+1 条连接不被拒绝，而是在内核 accept 队列排队，
// 有连接释放后立即被服务。
func TestLimitListenerQueues(t *testing.T) {
	base, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer base.Close()
	const limit = 3
	limited := netutil.LimitListener(base, limit)

	conns := make(chan net.Conn, 8)
	errCh := make(chan error, 1)
	acceptingLoop(limited, conns, errCh)

	// 占满 3 个配额：3 条拨入并被 Accept（服务端持住连接 = 占住槽位）。
	var held []net.Conn
	for i := 0; i < limit; i++ {
		if _, err := net.Dial("tcp", base.Addr().String()); err != nil {
			t.Fatalf("dial %d: %v", i, err)
		}
		select {
		case srv := <-conns:
			held = append(held, srv)
		case <-time.After(2 * time.Second):
			t.Fatalf("conn %d not accepted", i)
		}
	}

	// 第 4 条连接：TCP 建立成功（排队而非拒绝），但不会被 Accept。
	c4, err := net.DialTimeout("tcp", base.Addr().String(), time.Second)
	if err != nil {
		t.Fatalf("4th connection should queue (TCP established), got error: %v", err)
	}
	defer c4.Close()
	select {
	case srv := <-conns:
		t.Fatalf("4th connection must be queued, but got served: %v", srv.RemoteAddr())
	case <-time.After(300 * time.Millisecond):
	}

	// 释放一条 → 排队中的第 4 条立即被服务。
	held[0].Close()
	select {
	case srv := <-conns:
		srv.Close()
	case <-time.After(2 * time.Second):
		t.Fatal("queued connection should be served after a slot is released")
	}
	for _, c := range held[1:] {
		c.Close()
	}
}

// TestLimitListenerCloseUnblocksAccept R5 回归：配额满且 Accept 阻塞在
// acquire 时 Close listener，Accept 必须立即返回错误（x/net 实现带 done-channel
// 修复，与 Shutdown 无互锁——手写 semaphore 包 listener 的教训）。
func TestLimitListenerCloseUnblocksAccept(t *testing.T) {
	base, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	limited := netutil.LimitListener(base, 1)

	conns := make(chan net.Conn, 4)
	errCh := make(chan error, 1)
	acceptingLoop(limited, conns, errCh)

	// 占满唯一配额 → 第二轮 Accept 阻塞在 acquire。
	c1, err := net.Dial("tcp", base.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c1.Close()
	select {
	case srv := <-conns:
		defer srv.Close()
	case <-time.After(2 * time.Second):
		t.Fatal("first conn not accepted")
	}
	_, err = net.Dial("tcp", base.Addr().String()) // 触发下一次 Accept 尝试
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond) // 等 Accept 进入阻塞 acquire

	// Close listener（Shutdown 路径等价物）→ Accept 必须带错返回，不悬挂。
	if err := limited.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-errCh:
	case <-time.After(2 * time.Second):
		t.Fatal("Accept must return after listener close (no Shutdown interlock)")
	}
}

// newTestTLSListener 组装与 listenTLS 同构的监听链（R15-#4）：真实 pki 证书
// （`meshconsole pki` 同源 Ensure 生成）→ LimitListener → TLS。返回 listener、
// 基础 TCP 地址与服务端证书 PEM（客户端信任用）。
func newTestTLSListener(t *testing.T, limit int) (net.Listener, string, []byte) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "pki")
	res, err := pki.Ensure(pki.Config{Dir: dir})
	if err != nil {
		t.Fatalf("pki ensure: %v", err)
	}
	tlsCfg, err := pki.ServerTLSConfig(res.ServerCertPath, res.ServerKeyPath, "")
	if err != nil {
		t.Fatalf("server tls config: %v", err)
	}
	certPEM, err := os.ReadFile(res.ServerCertPath)
	if err != nil {
		t.Fatal(err)
	}
	base, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { base.Close() })
	return tls.NewListener(netutil.LimitListener(base, limit), tlsCfg), base.Addr().String(), certPEM
}

// tlsClient 构造信任给定服务端证书（自签叶子）的 HTTPS 客户端。
func tlsClient(t *testing.T, certPEM []byte) *http.Client {
	t.Helper()
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(certPEM) {
		t.Fatal("bad server cert pem")
	}
	return &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}}}
}

// TestDrainHTTPOverTLSLimitListener R15-#4：main 退出路径同款 drainHTTP 序列
// （Shutdown→超时强断→handler 归零）在「真实 TLS 证书 + LimitListener 监听链 +
// 一条在途连接」场景的回归——排空（在途请求被等完）或强断（宽限超时 Close）
// 后函数必须返回，且在途 handler 全部归零。不穷举满配额/未完成握手等组合。
func TestDrainHTTPOverTLSLimitListener(t *testing.T) {
	log := slog.New(slog.NewJSONHandler(io.Discard, nil))

	// 场景一（排空）：在途 handler 在宽限期内完成 → Shutdown 正常结束，
	// 客户端收到完整响应，drainHTTP（真实常量、与 main 逐字同款）返回。
	t.Run("graceful drain", func(t *testing.T) {
		ln, addr, certPEM := newTestTLSListener(t, 8)
		var inFlight sync.WaitGroup
		entered := make(chan struct{})
		release := make(chan struct{})
		root := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			inFlight.Add(1)
			defer inFlight.Done()
			close(entered)
			<-release
			w.WriteHeader(http.StatusOK)
		})
		srv := &http.Server{Handler: root, ReadHeaderTimeout: 5 * time.Second}
		go srv.Serve(ln)

		client := tlsClient(t, certPEM)
		type result struct {
			resp *http.Response
			err  error
		}
		resCh := make(chan result, 1)
		go func() {
			resp, err := client.Get("https://" + addr + "/slow")
			resCh <- result{resp, err}
		}()
		select {
		case <-entered:
		case <-time.After(5 * time.Second):
			t.Fatal("in-flight handler never entered")
		}

		done := make(chan struct{})
		go func() {
			drainHTTP(log, srv, &inFlight)
			close(done)
		}()
		close(release) // 宽限期内放行 → Shutdown 正常排空
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Fatal("drainHTTP must return after in-flight handler completes")
		}
		select {
		case got := <-resCh:
			if got.err != nil {
				t.Fatalf("in-flight request must be drained, got error: %v", got.err)
			}
			if got.resp.StatusCode != http.StatusOK {
				t.Fatalf("drained response status = %d, want 200", got.resp.StatusCode)
			}
			got.resp.Body.Close()
		case <-time.After(5 * time.Second):
			t.Fatal("client never received the drained response")
		}
	})

	// 场景二（强断）：handler 无视宽限持续阻塞 → Shutdown 超时（注入 300ms 宽限，
	// 序列与生产同款）→ srv.Close() 强断 → handler 随连接断开返回 →
	// WaitGroup 归零，函数返回；客户端连接被切断而非拿到干净响应。
	t.Run("forced close on timeout", func(t *testing.T) {
		ln, addr, certPEM := newTestTLSListener(t, 8)
		var inFlight sync.WaitGroup
		entered := make(chan struct{})
		release := make(chan struct{})
		root := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			inFlight.Add(1)
			defer inFlight.Done()
			close(entered)
			select {
			case <-release:
			case <-r.Context().Done(): // 强断关闭连接 → handler 快速返回
			}
		})
		srv := &http.Server{Handler: root, ReadHeaderTimeout: 5 * time.Second}
		go srv.Serve(ln)

		client := tlsClient(t, certPEM)
		type result struct {
			resp *http.Response
			err  error
		}
		resCh := make(chan result, 1)
		go func() {
			resp, err := client.Get("https://" + addr + "/stuck")
			resCh <- result{resp, err}
		}()
		select {
		case <-entered:
		case <-time.After(5 * time.Second):
			t.Fatal("in-flight handler never entered")
		}

		done := make(chan struct{})
		go func() {
			drainHTTPSeq(log, srv, &inFlight, 300*time.Millisecond, 5*time.Second)
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Fatal("drainHTTP must return after forced close")
		}
		select {
		case got := <-resCh:
			if got.err == nil {
				got.resp.Body.Close()
				t.Fatalf("connection should be force-closed, got clean response %d", got.resp.StatusCode)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("client neither failed nor received a response")
		}
		close(release) // 兜底放行，防 handler 悬挂
	})
}
