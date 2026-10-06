package server

import (
	"testing"
	"time"
)

const plBody = `{"code":11115,"msg":"prompt is too long: 1187903 tokens > 1048576 maximum"}`

// 学习式闸门只认「同模型 + body 不小于已被拒规模 + 按证据密度估算仍超顶」，宁漏不误杀。
func TestOversizeGateLearnsAndBlocks(t *testing.T) {
	used, limit, ok := parsePromptTooLong(plBody)
	if !ok || used != 1187903 || limit != 1048576 {
		t.Fatalf("parsePromptTooLong=(%d,%d,%v) want (1187903,1048576,true)", used, limit, ok)
	}
	if _, _, ok := parsePromptTooLong(`{"msg":"upstream busy"}`); ok {
		t.Fatal("unrelated body must not parse")
	}
	// 上限未知/实际未超上限的文案都不作证据。
	if _, _, ok := parsePromptTooLong(`{"msg":"prompt is too long: 100 tokens > 200 maximum"}`); ok {
		t.Fatal("used<=limit must not count as evidence")
	}

	g := &oversizeGate{}
	const model = "deepseek-v4.1-flash"
	if _, _, hit := g.blocked(model, 5_000_000); hit {
		t.Fatal("cold gate must not block anything")
	}
	// 证据：3,900,000 字节的 body 实际 1,187,903 tokens（≈0.3046 tokens/byte）。
	g.note(model, 3_900_000, used, limit)

	if est, lim, hit := g.blocked(model, 3_900_000); !hit || est <= lim {
		t.Fatalf("identical repeat: hit=%v est=%d limit=%d want blocked", hit, est, lim)
	}
	if _, _, hit := g.blocked(model, 5_000_000); !hit {
		t.Fatal("larger body must be blocked")
	}
	// 更小的 body 永不挡（哪怕估算刚好超顶）。
	if _, _, hit := g.blocked(model, 3_899_999); hit {
		t.Fatal("smaller body must never be blocked")
	}
	if _, _, hit := g.blocked("kimi-k3", 9_000_000); hit {
		t.Fatal("gate is per-model")
	}
	// 换证据后按最新密度走。
	g.note("dense", 1_000_000, 1_100_000, 1_048_576) // 1.1 tokens/byte
	if _, _, hit := g.blocked("dense", 1_000_000); !hit {
		t.Fatal("dense-content evidence must block same-size body")
	}
	// 估算未超顶（同密度但更小的 body 已在上面覆盖）；这里守 size 地板：更小一律放行。
	if _, _, hit := g.blocked("dense", 999_999); hit {
		t.Fatal("body below the proven-rejected size must not block")
	}
}

// 证据残缺（缺数字/零字节）不得武装闸门。
func TestOversizeGateIgnoresJunkEvidence(t *testing.T) {
	g := &oversizeGate{}
	g.note("", 10, 100, 50)
	g.note("m", 0, 100, 50)
	g.note("m", 1000, 0, 50)
	g.note("m", 1000, 100, 0)
	if _, _, hit := g.blocked("m", 1<<30); hit {
		t.Fatal("incomplete evidence must not arm the gate")
	}
}

func TestOversizeGateExpiry(t *testing.T) {
	old := oversizeGateTTL
	oversizeGateTTL = 20 * time.Millisecond
	defer func() { oversizeGateTTL = old }()

	g := &oversizeGate{}
	g.note("m", 1000, 1187903, 1048576)
	if _, _, hit := g.blocked("m", 1000); !hit {
		t.Fatal("fresh evidence must block")
	}
	time.Sleep(40 * time.Millisecond)
	if _, _, hit := g.blocked("m", 10_000); hit {
		t.Fatal("expired evidence must not block (client may have compacted)")
	}
}
