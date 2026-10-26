package main

import (
	"testing"

	utls "github.com/refraction-networking/utls"
)

// 抓包事实锁定：这两个数字变了，JA4 就变了，上游会重新 429。
func TestBunHelloShape(t *testing.T) {
	spec := bunHello()
	if n := len(spec.CipherSuites); n != 17 {
		t.Errorf("cipher 数 = %d，抓包事实是 17", n)
	}
	if spec.CipherSuites[0] != utls.TLS_AES_128_GCM_SHA256 {
		t.Errorf("首个 cipher = %#04x，应为 TLS_AES_128_GCM_SHA256", spec.CipherSuites[0])
	}
	if n := len(spec.Extensions); n != 13 {
		t.Errorf("扩展数 = %d，抓包事实是 13", n)
	}
	if _, ok := spec.Extensions[0].(*utls.SNIExtension); !ok {
		t.Errorf("首个扩展应为 SNI，得到 %T", spec.Extensions[0])
	}
	last := spec.Extensions[len(spec.Extensions)-1]
	if _, ok := last.(*utls.SupportedVersionsExtension); !ok {
		t.Errorf("最后扩展应为 supported_versions，得到 %T", last)
	}

	var alpn *utls.ALPNExtension
	for _, ext := range spec.Extensions {
		if a, ok := ext.(*utls.ALPNExtension); ok {
			alpn = a
		}
	}
	if alpn == nil {
		t.Fatal("缺 ALPN 扩展")
	}
	if len(alpn.AlpnProtocols) != 1 || alpn.AlpnProtocols[0] != "http/1.1" {
		t.Errorf("ALPN 只能是 http/1.1，得到 %v", alpn.AlpnProtocols)
	}

	var curves *utls.SupportedCurvesExtension
	for _, ext := range spec.Extensions {
		if c, ok := ext.(*utls.SupportedCurvesExtension); ok {
			curves = c
		}
	}
	if curves == nil || len(curves.Curves) == 0 {
		t.Fatal("缺 supported_groups 扩展")
	}
	if curves.Curves[0] != utls.X25519MLKEM768 {
		t.Errorf("首个 curve = %v，应为 X25519MLKEM768", curves.Curves[0])
	}
}

// 传输层默认值与 sidecar 的关系：原生传输接管后不再拉 bun，显式 on 仍然优先。
func TestSidecarDefaultFollowsTransport(t *testing.T) {
	t.Setenv("DIRECT_SIDECAR", "")
	t.Setenv("ZEN_TLS", "bun")
	if !sidecarOff() {
		t.Error("ZEN_TLS=bun 时默认不拉起 sidecar")
	}
	t.Setenv("ZEN_TLS", "go")
	if sidecarOff() {
		t.Error("ZEN_TLS=go 时应回落 sidecar 兜底指纹")
	}
	t.Setenv("DIRECT_SIDECAR", "on")
	if sidecarOff() {
		t.Error("显式 DIRECT_SIDECAR=on 应优先于传输层默认")
	}
}
