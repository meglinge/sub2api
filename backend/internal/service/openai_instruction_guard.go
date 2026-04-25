package service

import (
	"encoding/json"
	"fmt"
	"strings"
)

func resolveOpenAIInstructionGuardPrompt(account *Account) string {
	if account == nil {
		return ""
	}
	return strings.TrimSpace(account.GetCodexInstructionGuardPrompt())
}

func mergeOpenAIInstructionGuard(reqBody map[string]any, guardPrompt string) bool {
	guardPrompt = strings.TrimSpace(guardPrompt)
	if len(reqBody) == 0 || guardPrompt == "" {
		return false
	}

	existing, _ := reqBody["instructions"].(string)
	if strings.HasPrefix(strings.TrimSpace(existing), guardPrompt) {
		return false
	}
	if strings.TrimSpace(existing) == "" {
		reqBody["instructions"] = guardPrompt
		return true
	}
	reqBody["instructions"] = guardPrompt + "\n\n" + existing
	return true
}

func mergeOpenAIInstructionGuardToBody(body []byte, guardPrompt string) ([]byte, bool, error) {
	guardPrompt = strings.TrimSpace(guardPrompt)
	if len(body) == 0 || guardPrompt == "" {
		return body, false, nil
	}

	var reqBody map[string]any
	if err := json.Unmarshal(body, &reqBody); err != nil {
		return body, false, fmt.Errorf("parse request body for instruction guard: %w", err)
	}
	if !mergeOpenAIInstructionGuard(reqBody, guardPrompt) {
		return body, false, nil
	}
	merged, err := json.Marshal(reqBody)
	if err != nil {
		return body, false, fmt.Errorf("serialize request body for instruction guard: %w", err)
	}
	return merged, true, nil
}
