package server

import (
	"testing"
	"time"
)

// 学习式闸门只认「同模型 + body 不小于已被拒规模」，宁漏不误杀：
// 更小的请求、别的模型、冷启动都必须放行。
func TestOversizeGateLearnsAndBlocks(t *testing.T) {
	g := &oversizeGate{}
	const model = "deepseek-v4.1-flash"
	if _, hit := g.blocked(model, 5_000_000); hit {
		t.Fatal("cold gate must not block anything")
	}
	limit := promptTooLongLimit(`{"code":11115,"msg":"prompt is too long: 1187903 tokens > 1048576 maximum"}`)
	if limit != 1048576 {
		t.Fatalf("promptTooLongLimit=%d want 1048576", limit)
	}
	g.note(model, 4_000_000, limit)
	if got, hit := g.blocked(model, 4_000_000); !hit || got != 1048576 {
		t.Fatalf("equal-size body: hit=%v limit=%d want true/1048576", hit, got)
	}
	if _, hit := g.blocked(model, 4_000_001); !hit {
		t.Fatal("larger body must be blocked")
	}
	if _, hit := g.blocked(model, 3_999_999); hit {
		t.Fatal("smaller body must pass (no false positive)")
	}
	if _, hit := g.blocked("kimi-k3", 9_000_000); hit {
		t.Fatal("gate is per-model")
	}

	// 只收紧不放宽：更小的证据下调地板，更大的证据不得放松。
	g.note(model, 5_000_000, 0)
	if _, hit := g.blocked(model, 4_000_000); !hit {
		t.Fatal("larger evidence must not raise the floor")
	}
	g.note(model, 2_000_000, 0)
	if got, hit := g.blocked(model, 2_500_000); !hit || got != 1048576 {
		t.Fatalf("tighter evidence: hit=%v limit=%d want true/1048576 (limit persists)", hit, got)
	}
}

func TestOversizeGateIgnoresJunk(t *testing.T) {
	g := &oversizeGate{}
	if promptTooLongLimit(`{"msg":"upstream busy"}`) != 0 {
		t.Fatal("unrelated body must parse as unknown limit")
	}
	g.note("", 10, 5)
	g.note("m", 0, 5)
	if _, hit := g.blocked("m", 1 << 30); hit {
		t.Fatal("zero-length note must not arm the gate")
	}
}

func TestOversizeGateExpiry(t *testing.T) {
	old := oversizeGateTTL
	oversizeGateTTL = 20 * time.Millisecond
	defer func() { oversizeGateTTL = old }()

	g := &oversizeGate{}
	g.note("m", 1000, 0)
	if _, hit := g.blocked("m", 1000); !hit {
		t.Fatal("fresh entry must block")
	}
	time.Sleep(40 * time.Millisecond)
	if _, hit := g.blocked("m", 10_000); hit {
		t.Fatal("expired entry must not block (client may have compacted)")
	}
}
