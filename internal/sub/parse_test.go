package sub

import "testing"

func TestParseVLESS(t *testing.T) {
	link := "vless://11111111-1111-1111-1111-111111111111@example.com:443?encryption=none&security=tls&type=ws&path=%2Fws&host=example.com&sni=example.com#test"
	n, err := ParseLink(link)
	if err != nil {
		t.Fatal(err)
	}
	if n.Protocol != "vless" || n.Address != "example.com" || n.Port != 443 {
		t.Fatalf("unexpected node: %+v", n)
	}
	if n.Outbound["tag"] == nil {
		t.Fatal("missing tag")
	}
}

func TestParseVMess(t *testing.T) {
	// {"v":"2","ps":"n","add":"1.2.3.4","port":"443","id":"11111111-1111-1111-1111-111111111111","aid":"0","net":"tcp","type":"none","tls":"tls"}
	link := "vmess://eyJ2IjoiMiIsInBzIjoibiIsImFkZCI6IjEuMi4zLjQiLCJwb3J0IjoiNDQzIiwiaWQiOiIxMTExMTExMS0xMTExLTExMTEtMTExMS0xMTExMTExMTExMTEiLCJhaWQiOiIwIiwibmV0IjoidGNwIiwidHlwZSI6Im5vbmUiLCJ0bHMiOiJ0bHMifQ=="
	n, err := ParseLink(link)
	if err != nil {
		t.Fatal(err)
	}
	if n.Protocol != "vmess" || n.Address != "1.2.3.4" || n.Port != 443 {
		t.Fatalf("unexpected: %+v", n)
	}
}

func TestParseSS(t *testing.T) {
	link := "ss://YWVzLTI1Ni1nY206cGFzc3dvcmQ@1.2.3.4:8388#n"
	n, err := ParseLink(link)
	if err != nil {
		t.Fatal(err)
	}
	if n.Protocol != "shadowsocks" || n.Port != 8388 {
		t.Fatalf("unexpected: %+v", n)
	}
}
