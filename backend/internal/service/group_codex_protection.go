package service

import "strings"

// IsCodexProtectionEnabled 返回分组级 Codex 防破限保护是否启用。
// 仅 openai 平台分组生效。
func (g *Group) IsCodexProtectionEnabled() bool {
	return g != nil && g.Platform == PlatformOpenAI && g.CodexProtectionEnabled
}

// GetCodexInstructionGuardPrompt 返回分组级 Codex 防破限指令。
// 开关开启但未自定义 prompt 时，回退默认 guard 文本。
func (g *Group) GetCodexInstructionGuardPrompt() string {
	if !g.IsCodexProtectionEnabled() {
		return ""
	}
	if prompt := strings.TrimSpace(g.CodexInstructionGuardPrompt); prompt != "" {
		return prompt
	}
	return defaultCodexInstructionGuardPrompt
}

// GetCodexHardBlockReply 返回分组级 Codex 硬拦截固定回复。
// 开关开启但未自定义 reply 时，回退默认拒绝文案。
func (g *Group) GetCodexHardBlockReply() string {
	if !g.IsCodexProtectionEnabled() {
		return ""
	}
	if reply := strings.TrimSpace(g.CodexHardBlockReply); reply != "" {
		return reply
	}
	return defaultCodexHardBlockReply
}

func sanitizeGroupCodexProtectionFields(g *Group) {
	if g == nil || g.Platform == PlatformOpenAI {
		return
	}
	g.CodexProtectionEnabled = false
	g.CodexInstructionGuardPrompt = ""
	g.CodexHardBlockReply = ""
}
