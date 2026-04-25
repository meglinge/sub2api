-- Add group-level Codex protection controls for OpenAI groups.
-- One toggle enables both instruction guard injection and local hard block.
ALTER TABLE groups
    ADD COLUMN IF NOT EXISTS codex_protection_enabled boolean NOT NULL DEFAULT false,
    ADD COLUMN IF NOT EXISTS codex_instruction_guard_prompt text,
    ADD COLUMN IF NOT EXISTS codex_hard_block_reply text;

COMMENT ON COLUMN groups.codex_protection_enabled IS '是否对本分组所有 OpenAI OAuth 账号启用 Codex 防破限保护。';
COMMENT ON COLUMN groups.codex_instruction_guard_prompt IS '分组级 Codex 防破限指令；为空时使用默认提示词。';
COMMENT ON COLUMN groups.codex_hard_block_reply IS '命中分组级 Codex 硬拦截时的固定回复；为空时使用默认回复。';
