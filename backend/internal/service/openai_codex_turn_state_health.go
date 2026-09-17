package service

import (
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
)

var (
	errInvalidCodexTurnStateProxyScheme = errors.New("ipv6_proxy_url scheme must be socks5, socks5h, http, or https")
	errCodexTurnStateNotFernet          = errors.New("turn-state 不是 Fernet 令牌")
)

type skipStoredCodexTurnStateKey struct{}

type CodexTurnStateHealth struct {
	CipherLen         int    `json:"cipher_len"`
	ExpectedCipherLen int    `json:"expected_cipher_len"`
	Degraded          bool   `json:"degraded"`
	Error             string `json:"error,omitempty"`
}

type CodexTurnStateDegradedError struct {
	Health CodexTurnStateHealth
}

func (e *CodexTurnStateDegradedError) Error() string {
	return fmt.Sprintf("智力校验未通过: Fernet 密文 %d 字节（降智），期望 %d", e.Health.CipherLen, e.Health.ExpectedCipherLen)
}

type codexTurnStateTokenInfo struct {
	Version   byte
	Timestamp int64
	CipherLen int
}

func InspectCodexTurnStateHealth(value, planType string) *CodexTurnStateHealth {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	health := &CodexTurnStateHealth{ExpectedCipherLen: codexTurnStateHealthyCipherLenForPlan(planType)}
	info, err := inspectCodexTurnStateToken(value)
	if err != nil {
		health.Error = err.Error()
		return health
	}
	health.CipherLen = info.CipherLen
	health.Degraded = info.CipherLen != health.ExpectedCipherLen
	return health
}

func verifyCodexTurnStatePingIntelligence(value, planType string) error {
	health := InspectCodexTurnStateHealth(value, planType)
	if health == nil {
		return errors.New("turn-state 为空")
	}
	if health.Error != "" {
		return errors.New(health.Error)
	}
	if health.Degraded {
		return &CodexTurnStateDegradedError{Health: *health}
	}
	return nil
}

func codexTurnStateHealthyCipherLenForPlan(planType string) int {
	switch strings.ToLower(strings.TrimSpace(planType)) {
	case "team", "k12", "teamplus", "self_serve_business", "self_serve_business_prolite", "self_serve_business_plus", "enterprise":
		return codexTurnStateTeamHealthyCipherLen
	default:
		if strings.HasPrefix(strings.ToLower(strings.TrimSpace(planType)), "self_serve_business") {
			return codexTurnStateTeamHealthyCipherLen
		}
		return codexTurnStateHealthyCipherLen
	}
}

func inspectCodexTurnStateToken(value string) (codexTurnStateTokenInfo, error) {
	raw, err := decodeCodexTurnStateFernet(value)
	if err != nil {
		return codexTurnStateTokenInfo{}, err
	}
	const header = 1 + 8 + 16
	const hmacLen = 32
	if len(raw) < header+hmacLen || raw[0] != codexTurnStateFernetVersion {
		return codexTurnStateTokenInfo{}, errCodexTurnStateNotFernet
	}
	cipherLen := len(raw) - header - hmacLen
	if cipherLen <= 0 {
		return codexTurnStateTokenInfo{}, errCodexTurnStateNotFernet
	}
	ts := int64(raw[1])<<56 | int64(raw[2])<<48 | int64(raw[3])<<40 | int64(raw[4])<<32 |
		int64(raw[5])<<24 | int64(raw[6])<<16 | int64(raw[7])<<8 | int64(raw[8])
	return codexTurnStateTokenInfo{
		Version:   raw[0],
		Timestamp: ts,
		CipherLen: cipherLen,
	}, nil
}

func decodeCodexTurnStateFernet(value string) ([]byte, error) {
	value = strings.TrimSpace(value)
	if decoded, err := base64.URLEncoding.DecodeString(value); err == nil {
		return decoded, nil
	}
	if decoded, err := base64.RawURLEncoding.DecodeString(value); err == nil {
		return decoded, nil
	}
	if decoded, err := base64.StdEncoding.DecodeString(value); err == nil {
		return decoded, nil
	}
	return base64.RawStdEncoding.DecodeString(value)
}
