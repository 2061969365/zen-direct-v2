package main

// bunTransport：把请求按 Bun fetch 的样子放到线上。
//
// 为什么不直接用 http.Transport：
//   - crypto/tls 永远发 Go 自己的 ClientHello，上游按指纹直接 429；
//   - net/http 的 HTTP/1.1 writer 有自己的头顺序与大小写规则。
//
// 所以这里自己完成三件事：uTLS 握手（bunHello）、HTTP/1.1 only、
// 按 fetch 的写法渲染请求头（bunHead），并自己管连接池。
// 解压只针对「Accept-Encoding 是本层补的」这种情况，调用方自己给了就原样透传。
//
// 参考实现（仅对照，不参与编译）：reference/_opencode2api/httpx/。

import (
	"bufio"
	"compress/flate"
	"compress/gzip"
	"compress/zlib"
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/andybalholm/brotli"
	"github.com/klauspost/compress/zstd"
	utls "github.com/refraction-networking/utls"
)

const (
	bunHandshakeTimeout = 10 * time.Second
	bunMaxIdlePerHost   = 20
)

// bunTransport 实现 http.RoundTripper。用 newBunTransport 构造。
type bunTransport struct {
	proxy           *url.URL
	dialer          *net.Dialer
	responseTimeout time.Duration
	idleTimeout     time.Duration
	maxIdlePerHost  int

	mu   sync.Mutex
	idle map[string][]*bunConn
}

// bunConn 是一条上游连接，带读缓冲（响应解析）与所属池键。
type bunConn struct {
	net.Conn
	key   string
	br    *bufio.Reader
	timer *time.Timer
}

// newBunTransport 构造传输层。proxy 为 nil 时直连；responseTimeout 是
// 「发出请求到收到响应头」的上限；idleTimeout 是连接池里空闲连接的最长寿命。
func newBunTransport(proxy *url.URL, responseTimeout, idleTimeout time.Duration) *bunTransport {
	if idleTimeout <= 0 {
		idleTimeout = 60 * time.Second
	}
	return &bunTransport{
		proxy:           proxy,
		dialer:          &net.Dialer{Timeout: 20 * time.Second, KeepAlive: 30 * time.Second},
		responseTimeout: responseTimeout,
		idleTimeout:     idleTimeout,
		maxIdlePerHost:  bunMaxIdlePerHost,
		idle:            map[string][]*bunConn{},
	}
}

// proxyFromEnvironment 复刻 net/http 的代理发现（HTTPS_PROXY / NO_PROXY 等）。
func proxyFromEnvironment() *url.URL {
	probe := &http.Request{URL: &url.URL{Scheme: "https", Host: "opencode.ai"}}
	u, err := http.ProxyFromEnvironment(probe)
	if err != nil {
		return nil
	}
	return u
}

// RoundTrip 实现 http.RoundTripper。
func (t *bunTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	body, err := bunReadBody(req)
	if err != nil {
		return nil, err
	}
	u := req.URL
	if u == nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
		return nil, fmt.Errorf("bun transport: unsupported url %v", u)
	}
	head, addedEncoding, err := bunHead(req, body)
	if err != nil {
		return nil, err
	}
	port := u.Port()
	if port == "" {
		if u.Scheme == "https" {
			port = "443"
		} else {
			port = "80"
		}
	}
	addr := net.JoinHostPort(u.Hostname(), port)
	key := u.Scheme + "://" + addr

	for attempt := 0; ; attempt++ {
		conn, reused, err := t.conn(req.Context(), key, addr, u.Hostname(), u.Scheme)
		if err != nil {
			return nil, err
		}
		resp, dropped, err := t.exchange(req, conn, head, body, addedEncoding)
		if err == nil {
			return resp, nil
		}
		// 池里捞出来的连接可能已被服务端悄悄关掉：没读到任何响应字节时换新连接重试一次。
		if reused && dropped && attempt == 0 && req.Context().Err() == nil {
			continue
		}
		return nil, err
	}
}

// CloseIdleConnections 关掉池里所有空闲连接。
func (t *bunTransport) CloseIdleConnections() {
	t.mu.Lock()
	conns := t.idle
	t.idle = map[string][]*bunConn{}
	t.mu.Unlock()
	for _, list := range conns {
		for _, c := range list {
			if c.timer != nil {
				c.timer.Stop()
			}
			c.Close()
		}
	}
}

func bunReadBody(req *http.Request) ([]byte, error) {
	if req.Body == nil || req.Body == http.NoBody {
		return nil, nil
	}
	defer req.Body.Close()
	return io.ReadAll(req.Body)
}

// conn 取一条连接：优先复用池里的，否则新建（必要时先过代理，再做 uTLS 握手）。
func (t *bunTransport) conn(ctx context.Context, key, addr, serverName, scheme string) (*bunConn, bool, error) {
	if c := t.takeIdle(key); c != nil {
		return c, true, nil
	}
	raw, err := t.dial(ctx, addr)
	if err != nil {
		return nil, false, err
	}
	if scheme == "https" {
		tlsConn := utls.UClient(raw, &utls.Config{ServerName: serverName}, utls.HelloCustom)
		if err := tlsConn.ApplyPreset(bunHello()); err != nil {
			raw.Close()
			return nil, false, err
		}
		hsCtx, cancel := context.WithTimeout(ctx, bunHandshakeTimeout)
		err = tlsConn.HandshakeContext(hsCtx)
		cancel()
		if err != nil {
			raw.Close()
			return nil, false, err
		}
		raw = tlsConn
	}
	return &bunConn{Conn: raw, key: key, br: bufio.NewReader(raw)}, false, nil
}

// dial 建立到 addr 的 TCP 连接，按需穿过代理。
func (t *bunTransport) dial(ctx context.Context, addr string) (net.Conn, error) {
	if t.proxy == nil {
		return t.dialer.DialContext(ctx, "tcp", addr)
	}
	return t.dialProxy(ctx, addr)
}

// dialProxy 通过代理开隧道：http/https 走 CONNECT（https 代理本身要再套一层 TLS），
// socks5/socks5h 走 SOCKS5 CONNECT。返回的 *net.OpError 与 net/http 同形，
// 便于调用方区分「代理失败」与「上游失败」。
func (t *bunTransport) dialProxy(ctx context.Context, addr string) (net.Conn, error) {
	socks := t.proxy.Scheme == "socks5" || t.proxy.Scheme == "socks5h"
	op := "proxyconnect"
	if socks {
		op = "socks connect"
	}
	conn, err := t.dialer.DialContext(ctx, "tcp", bunProxyAddr(t.proxy))
	if err != nil {
		return nil, &net.OpError{Op: op, Net: "tcp", Err: err}
	}
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	_ = conn.SetDeadline(time.Now().Add(bunHandshakeTimeout))
	if t.proxy.Scheme == "https" {
		tlsConn := tls.Client(conn, &tls.Config{ServerName: t.proxy.Hostname()})
		if err = tlsConn.HandshakeContext(ctx); err == nil {
			conn = tlsConn
		}
	}
	if err == nil {
		if socks {
			err = bunSocks5(conn, t.proxy.User, addr)
		} else {
			conn, err = bunConnect(conn, t.proxy.User, addr)
		}
	}
	if !stop() && err == nil {
		err = ctx.Err()
	}
	if err == nil {
		err = conn.SetDeadline(time.Time{})
	}
	if err != nil {
		conn.Close()
		return nil, &net.OpError{Op: op, Net: "tcp", Err: err}
	}
	return conn, nil
}

func bunProxyAddr(proxy *url.URL) string {
	port := proxy.Port()
	if port == "" {
		switch proxy.Scheme {
		case "https":
			port = "443"
		case "socks5", "socks5h":
			port = "1080"
		default:
			port = "80"
		}
	}
	return net.JoinHostPort(proxy.Hostname(), port)
}

func bunConnect(conn net.Conn, user *url.Userinfo, addr string) (net.Conn, error) {
	req := "CONNECT " + addr + " HTTP/1.1\r\nHost: " + addr + "\r\n"
	if user != nil {
		password, _ := user.Password()
		cred := base64.StdEncoding.EncodeToString([]byte(user.Username() + ":" + password))
		req += "Proxy-Authorization: Basic " + cred + "\r\n"
	}
	if _, err := io.WriteString(conn, req+"\r\n"); err != nil {
		return conn, err
	}
	reader := bufio.NewReader(conn)
	resp, err := http.ReadResponse(reader, &http.Request{Method: http.MethodConnect})
	if err != nil {
		return conn, err
	}
	if resp.StatusCode/100 != 2 {
		return conn, fmt.Errorf("proxy refused CONNECT: %s", resp.Status)
	}
	if reader.Buffered() > 0 {
		return &bunBufferedConn{Conn: conn, reader: reader}, nil
	}
	return conn, nil
}

// bunBufferedConn 让代理在 CONNECT 应答后多写的字节不丢。
type bunBufferedConn struct {
	net.Conn
	reader *bufio.Reader
}

func (c *bunBufferedConn) Read(p []byte) (int, error) { return c.reader.Read(p) }

var bunSocksReplies = map[byte]string{
	1: "general SOCKS server failure",
	2: "connection not allowed by ruleset",
	3: "network unreachable",
	4: "host unreachable",
	5: "connection refused",
	6: "TTL expired",
	7: "command not supported",
	8: "address type not supported",
}

// bunSocks5 在已建立的 TCP 连接上做 SOCKS5 认证 + CONNECT。
func bunSocks5(conn net.Conn, user *url.Userinfo, addr string) error {
	host, portText, err := net.SplitHostPort(addr)
	if err != nil {
		return err
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		return err
	}
	methods := []byte{0}
	if user != nil {
		methods = append(methods, 2)
	}
	greeting := append([]byte{5, byte(len(methods))}, methods...)
	if _, err := conn.Write(greeting); err != nil {
		return err
	}
	reply := make([]byte, 2)
	if _, err := io.ReadFull(conn, reply); err != nil {
		return err
	}
	if reply[0] != 5 {
		return fmt.Errorf("unexpected SOCKS version %d", reply[0])
	}
	switch reply[1] {
	case 0:
	case 2:
		if user == nil {
			return errors.New("SOCKS server requires authentication")
		}
		password, _ := user.Password()
		auth := []byte{1, byte(len(user.Username()))}
		auth = append(auth, user.Username()...)
		auth = append(auth, byte(len(password)))
		auth = append(auth, password...)
		if _, err := conn.Write(auth); err != nil {
			return err
		}
		if _, err := io.ReadFull(conn, reply); err != nil {
			return err
		}
		if reply[1] != 0 {
			return errors.New("SOCKS authentication failed")
		}
	default:
		return fmt.Errorf("SOCKS server rejected methods: %d", reply[1])
	}

	request := []byte{5, 1, 0}
	if ip := net.ParseIP(host); ip != nil {
		if v4 := ip.To4(); v4 != nil {
			request = append(request, 1)
			request = append(request, v4...)
		} else {
			request = append(request, 4)
			request = append(request, ip.To16()...)
		}
	} else {
		if len(host) > 255 {
			return errors.New("SOCKS host name too long")
		}
		request = append(request, 3, byte(len(host)))
		request = append(request, host...)
	}
	request = append(request, byte(port>>8), byte(port))
	if _, err := conn.Write(request); err != nil {
		return err
	}

	header := make([]byte, 4)
	if _, err := io.ReadFull(conn, header); err != nil {
		return err
	}
	if header[0] != 5 {
		return fmt.Errorf("unexpected SOCKS version %d", header[0])
	}
	if header[1] != 0 {
		if text, ok := bunSocksReplies[header[1]]; ok {
			return errors.New(text)
		}
		return fmt.Errorf("unknown SOCKS reply %d", header[1])
	}
	var skip int
	switch header[3] {
	case 1:
		skip = net.IPv4len
	case 4:
		skip = net.IPv6len
	case 3:
		length := make([]byte, 1)
		if _, err := io.ReadFull(conn, length); err != nil {
			return err
		}
		skip = int(length[0])
	default:
		return fmt.Errorf("unknown SOCKS address type %d", header[3])
	}
	_, err = io.ReadFull(conn, make([]byte, skip+2))
	return err
}

// exchange 在一条连接上写完整请求并读出响应头。成功时响应体接管这条连接；
// 失败时关闭连接，并报告「是否在收到响应首字节前被对端断开」（可安全重试）。
func (t *bunTransport) exchange(req *http.Request, c *bunConn, head, body []byte, addedEncoding bool) (*http.Response, bool, error) {
	stop := context.AfterFunc(req.Context(), func() { c.Close() })
	fail := func(err error, dropped bool) (*http.Response, bool, error) {
		stop()
		c.Close()
		return nil, dropped, err
	}

	wire := make([]byte, 0, len(head)+len(body))
	wire = append(wire, head...)
	wire = append(wire, body...)
	if _, err := c.Write(wire); err != nil {
		return fail(err, bunDropped(err))
	}
	if t.responseTimeout > 0 {
		_ = c.SetReadDeadline(time.Now().Add(t.responseTimeout))
	}
	if _, err := c.br.Peek(1); err != nil {
		return fail(err, bunDropped(err))
	}
	resp, err := http.ReadResponse(c.br, req)
	if err != nil {
		return fail(err, false)
	}
	_ = c.SetReadDeadline(time.Time{})

	reusable := !resp.Close && !req.Close
	raw := &bunBody{t: t, c: c, src: resp.Body, reusable: reusable, stop: stop}
	if resp.Body == http.NoBody {
		raw.finish(true)
		return resp, false, nil
	}
	resp.Body = raw
	if addedEncoding {
		if encoding := strings.ToLower(strings.TrimSpace(resp.Header.Get("Content-Encoding"))); bunDecodable(encoding) {
			if decoder, ok := bunDecoder(encoding, raw); ok {
				resp.Body = decoder
				resp.Header.Del("Content-Encoding")
				resp.Header.Del("Content-Length")
				resp.ContentLength = -1
				resp.Uncompressed = true
			}
		}
	}
	return resp, false, nil
}

func bunDropped(err error) bool {
	return errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.ECONNABORTED) ||
		errors.Is(err, syscall.EPIPE)
}

func bunDecodable(encoding string) bool {
	switch encoding {
	case "gzip", "x-gzip", "deflate", "br", "zstd":
		return true
	}
	return false
}

func bunDecoder(encoding string, src io.ReadCloser) (io.ReadCloser, bool) {
	switch encoding {
	case "gzip", "x-gzip":
		r, err := gzip.NewReader(src)
		if err != nil {
			return nil, false
		}
		return r, true
	case "deflate":
		// 历史上 deflate 有 zlib 包装与裸流两种写法，先按 zlib 试。
		zr, err := zlib.NewReader(src)
		if err == nil {
			return zr, true
		}
		return flate.NewReader(src), true
	case "br":
		return io.NopCloser(brotli.NewReader(src)), true
	case "zstd":
		zr, err := zstd.NewReader(src)
		if err != nil {
			return nil, false
		}
		return &bunZstdBody{decoder: zr, src: src}, true
	}
	return nil, false
}

type bunZstdBody struct {
	decoder *zstd.Decoder
	src     io.ReadCloser
}

func (b *bunZstdBody) Read(p []byte) (int, error) { return b.decoder.Read(p) }
func (b *bunZstdBody) Close() error {
	b.decoder.Close()
	return b.src.Close()
}

// bunBody 是响应体，读干净后把连接放回池子；中途关闭/出错则丢弃连接。
type bunBody struct {
	t        *bunTransport
	c        *bunConn
	src      io.ReadCloser
	reusable bool
	stop     func() bool
	done     bool
}

func (b *bunBody) Read(p []byte) (int, error) {
	n, err := b.src.Read(p)
	if err == io.EOF {
		b.finish(true)
	} else if err != nil {
		b.finish(false)
	}
	return n, err
}

func (b *bunBody) Close() error {
	if !b.done {
		_ = b.src.Close()
		b.finish(false)
	}
	return nil
}

func (b *bunBody) finish(clean bool) {
	if b.done {
		return
	}
	b.done = true
	b.stop()
	if clean && b.reusable {
		b.t.putIdle(b.c)
		return
	}
	b.c.Close()
}

func (t *bunTransport) takeIdle(key string) *bunConn {
	t.mu.Lock()
	defer t.mu.Unlock()
	list := t.idle[key]
	if len(list) == 0 {
		return nil
	}
	c := list[len(list)-1]
	t.idle[key] = list[:len(list)-1]
	if c.timer != nil {
		c.timer.Stop()
		c.timer = nil
	}
	return c
}

func (t *bunTransport) putIdle(c *bunConn) {
	t.mu.Lock()
	if len(t.idle[c.key]) >= t.maxIdlePerHost {
		t.mu.Unlock()
		c.Close()
		return
	}
	if t.idleTimeout > 0 {
		c.timer = time.AfterFunc(t.idleTimeout, func() {
			if t.removeIdle(c) {
				c.Close()
			}
		})
	}
	t.idle[c.key] = append(t.idle[c.key], c)
	t.mu.Unlock()
}

func (t *bunTransport) removeIdle(c *bunConn) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	list := t.idle[c.key]
	for i, item := range list {
		if item == c {
			t.idle[c.key] = append(list[:i], list[i+1:]...)
			return true
		}
	}
	return false
}
