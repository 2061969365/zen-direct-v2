package main

import (
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
)

// bunWebkitHeaders 是 fetch/WebKit 认识的名字，fetch 会按这个顺序、这个
// 拼写把它们写在最前面（其余头一律小写、按字典序排在后面）。
//
// 顺序本身是指纹的一部分：上游看到的是「头出现的次序 + 大小写」，
// 与 TLS ClientHello 一起构成客户端画像。
var bunWebkitHeaders = []string{
	"Accept", "Accept-Charset", "Accept-Encoding", "Accept-Language", "Authorization", "Cache-Control",
	"Connection", "Content-Encoding", "Content-Language", "Content-Length", "Content-Type", "Cookie", "DNT",
	"Expect", "Host", "If-None-Match", "Keep-Alive", "Last-Event-Id", "Origin", "Pragma",
	"Proxy-Authorization", "Range", "Referer", "Sec-Fetch-Mode", "TE", "Upgrade-Insecure-Requests",
	"User-Agent",
}

// bunDefaultUA 是 fetch 在调用方没给 User-Agent 时补的值。zen-direct 的上游
// 请求总是自带 OpenCode 的 UA，这个默认值只在裸请求下出现，保持与运行时一致。
const bunDefaultUA = "Bun/1.4.3"

// bunAcceptEncoding 是 fetch 默认的 Accept-Encoding。带 br/zstd，服务端会压，
// 所以由 bunTransport 负责解压（只在自己添加这个头时）。
const bunAcceptEncoding = "gzip, deflate, br, zstd"

// bunHead 按 fetch 的写法渲染请求行与请求头。
//
// 顺序：
//  1. bunWebkitHeaders 中出现的头（大小写按上表）；
//  2. 其余头：名字小写、按字典序；
//  3. fetch 自己补的头：Connection、User-Agent（缺省）、Accept（缺省）、
//     Host、Accept-Encoding（缺省）、Content-Length（有 body 且未写过）。
//
// body 为 nil 表示无请求体；Content-Length 由本函数计算，调用方给的值忽略。
// 返回的 addedEncoding 表示 Accept-Encoding 是本函数补的，响应需要解压。
func bunHead(req *http.Request, body []byte) (head []byte, addedEncoding bool, err error) {
	values := make(map[string]string, len(req.Header))
	for name, list := range req.Header {
		for _, value := range list {
			if !bunValidField(name, value) {
				return nil, false, fmt.Errorf("bun transport: invalid header field %q", name)
			}
		}
		lower := strings.ToLower(name)
		switch lower {
		case "host", "connection", "content-length", "transfer-encoding":
			continue
		}
		if len(list) == 0 {
			continue
		}
		value := strings.Join(list, ", ")
		if prev, ok := values[lower]; ok {
			value = prev + ", " + value
		}
		values[lower] = value
	}

	// fetch 补的 Accept 默认值落在固定位置；调用方显式给 "*/*" 时等同一个默认值。
	_, hasUA := values["user-agent"]
	_, hasAccept := values["accept"]
	_, hasEncoding := values["accept-encoding"]
	if values["accept"] == "*/*" {
		delete(values, "accept")
		hasAccept = false
	}

	method := req.Method
	if method == "" {
		method = http.MethodGet
	}

	var b strings.Builder
	b.WriteString(method)
	b.WriteByte(' ')
	b.WriteString(req.URL.RequestURI())
	b.WriteString(" HTTP/1.1\r\n")

	wroteLength := false
	for _, name := range bunWebkitHeaders {
		lower := strings.ToLower(name)
		if lower == "content-length" {
			if body != nil {
				b.WriteString("Content-Length: ")
				b.WriteString(strconv.Itoa(len(body)))
				b.WriteString("\r\n")
				delete(values, lower)
				wroteLength = true
			}
			continue
		}
		if value, ok := values[lower]; ok {
			b.WriteString(name)
			b.WriteString(": ")
			b.WriteString(value)
			b.WriteString("\r\n")
			delete(values, lower)
		}
	}

	rest := make([]string, 0, len(values))
	for name := range values {
		rest = append(rest, name)
	}
	sort.Strings(rest)
	for _, name := range rest {
		b.WriteString(name)
		b.WriteString(": ")
		b.WriteString(values[name])
		b.WriteString("\r\n")
	}

	b.WriteString("Connection: keep-alive\r\n")
	if !hasUA {
		b.WriteString("User-Agent: " + bunDefaultUA + "\r\n")
	}
	if !hasAccept {
		b.WriteString("Accept: */*\r\n")
	}

	host := req.Host
	if host == "" {
		host = req.URL.Host
	}
	switch req.URL.Scheme {
	case "https":
		host = strings.TrimSuffix(host, ":443")
	case "http":
		host = strings.TrimSuffix(host, ":80")
	}
	b.WriteString("Host: " + host + "\r\n")

	if !hasEncoding {
		b.WriteString("Accept-Encoding: " + bunAcceptEncoding + "\r\n")
		addedEncoding = true
	}
	if body != nil && !wroteLength {
		b.WriteString("Content-Length: " + strconv.Itoa(len(body)) + "\r\n")
	}
	b.WriteString("\r\n")
	return []byte(b.String()), addedEncoding, nil
}

// bunValidField 校验 HTTP 头的名字/值，避免拼装时被注入分隔符。
func bunValidField(name, value string) bool {
	if name == "" {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		if c <= ' ' || c >= 0x7f || strings.IndexByte("\"(),/:;<=>?@[\\]{}", c) >= 0 {
			return false
		}
	}
	for i := 0; i < len(value); i++ {
		c := value[i]
		if (c < ' ' && c != '\t') || c == 0x7f {
			return false
		}
	}
	return true
}
