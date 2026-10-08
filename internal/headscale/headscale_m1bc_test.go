package headscale

// M1b-c §4 顺手修：节点地址字段名双代兼容（addresses / ipAddresses）。
// 回归背景：旧版 headscale 的 proto 字段为 ip_addresses（JSON ipAddresses），
// 旧解析只认 addresses 导致 tailnet ips=null。

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// legacyJSON 旧版风格：地址在 ipAddresses 下（无 addresses 键）。
const legacyJSON = `{
  "nodes": [
    {
      "id": 1,
      "name": "cloud-1",
      "ipAddresses": ["100.64.0.1", "fd7a:115c:a1e0::1"],
      "online": true
    },
    {
      "id": 2,
      "name": "mac-mini",
      "ipAddresses": ["100.64.0.2"]
    }
  ]
}`

// TestFetchNodesLegacyIPAddresses 旧字段名解析入库（ips=null 回归）。
func TestFetchNodesLegacyIPAddresses(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, legacyJSON)
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "k")
	nodes, err := c.FetchNodes(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) != 2 {
		t.Fatalf("nodes = %d, want 2", len(nodes))
	}
	if len(nodes[0].IPs) != 2 || nodes[0].IPs[0] != "100.64.0.1" {
		t.Fatalf("node0 ips = %v (legacy ipAddresses must parse)", nodes[0].IPs)
	}
	if len(nodes[1].IPs) != 1 || nodes[1].IPs[0] != "100.64.0.2" {
		t.Fatalf("node1 ips = %v", nodes[1].IPs)
	}
	// 全链路：Sync 后库内 ips 可读（原 bug 的观测点：面板 ips=null）。
	st := newTestStore(t)
	if _, err := NewFetcher(c, st).Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	rows := tailnetRows(t, st)
	if len(rows) != 2 || rows[0].IPs != "100.64.0.1,fd7a:115c:a1e0::1" {
		t.Fatalf("stored ips = %+v", rows)
	}
}

// TestFetchNodesAddressesPreferred 两代字段同现时优先新字段 addresses（不拼接）。
func TestFetchNodesAddressesPreferred(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"nodes":[{"id":1,"name":"n1","addresses":["100.64.9.9"],"ipAddresses":["10.0.0.1"]}]}`)
	}))
	defer srv.Close()

	nodes, err := NewClient(srv.URL, "k").FetchNodes(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes[0].IPs) != 1 || nodes[0].IPs[0] != "100.64.9.9" {
		t.Fatalf("ips = %v, want addresses preferred", nodes[0].IPs)
	}
}
