package service

import (
	"testing"
)

func TestPromptLooksTruncated(t *testing.T) {
	t.Parallel()
	if promptLooksTruncated(2536, 66740) != true {
		t.Fatal("60KB snap with 2536 tok must look truncated")
	}
	if promptLooksTruncated(27000, 66740) {
		t.Fatal("full prompt must not look truncated")
	}
	if promptLooksTruncated(2000, 1000) {
		t.Fatal("tiny request is not a dropped snapshot")
	}
}

func TestDecisionLooksLikeMissingSnapshot(t *testing.T) {
	t.Parallel()
	d := decision{Summary: "未提供账号快照，无法安全调整流量。", Notices: []any{"请提供本轮 snapshot 后再自动决策。"}}
	if !decisionLooksLikeMissingSnapshot(d) {
		t.Fatal("expected missing snapshot")
	}
	if decisionLooksLikeMissingSnapshot(decision{Summary: "主层收敛到3个低价号"}) {
		t.Fatal("normal summary must not match")
	}
}

func TestParseDecision_ObservationsStringArray(t *testing.T) {
	t.Parallel()
	// Production failure: cannot unmarshal string into map[string]interface{}
	raw := `{"summary":"ok","actions":[],"observations":["账号1 错误率高","分组2 可用偏低"],"notices":[]}`
	d, err := parseDecision(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if d.Summary != "ok" {
		t.Fatalf("summary=%q", d.Summary)
	}
	if len(d.Observations) != 2 {
		t.Fatalf("observations len=%d", len(d.Observations))
	}
}

func TestParseDecision_ObservationsObjects(t *testing.T) {
	t.Parallel()
	raw := `{"summary":"fine","actions":[{"accountId":12,"op":"set_weight","value":20,"reason":"recover","confidence":0.9}],"observations":[{"group":"1","note":"stable"}]}`
	d, err := parseDecision(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(d.Actions) != 1 {
		t.Fatalf("actions=%d", len(d.Actions))
	}
	if d.Actions[0].AccountID != 12 || d.Actions[0].Op != "set_weight" || d.Actions[0].Value != "20" {
		t.Fatalf("action=%+v", d.Actions[0])
	}
	if d.Actions[0].Confidence != 0.9 {
		t.Fatalf("confidence=%v", d.Actions[0].Confidence)
	}
}

func TestParseDecision_CodeFenceAndChannelId(t *testing.T) {
	t.Parallel()
	raw := "```json\n{\"summary\":\"x\",\"actions\":[{\"channelId\":5,\"op\":\"disable\",\"value\":\"\",\"reason\":\"bad\",\"confidence\":0.85}],\"observations\":[]}\n```"
	d, err := parseDecision(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(d.Actions) != 1 || d.Actions[0].AccountID != 5 {
		t.Fatalf("action=%+v", d.Actions)
	}
}

func TestChatCompletionsURL(t *testing.T) {
	t.Parallel()
	if got := chatCompletionsURL("https://api.openai.com/v1"); got != "https://api.openai.com/v1/chat/completions" {
		t.Fatalf("with /v1: %s", got)
	}
	if got := chatCompletionsURL("https://api.aixhan.com"); got != "https://api.aixhan.com/v1/chat/completions" {
		t.Fatalf("without /v1: %s", got)
	}
	if got := chatCompletionsURL("https://api.aixhan.com/"); got != "https://api.aixhan.com/v1/chat/completions" {
		t.Fatalf("trailing slash: %s", got)
	}
}
