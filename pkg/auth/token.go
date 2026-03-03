package auth

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strings"
)

var subdomainPattern = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$`)

type TokenEntry struct {
	Token     string `json:"token"`
	Subdomain string `json:"subdomain,omitempty"`
}

type tokenFile struct {
	Tokens []TokenEntry `json:"tokens"`
}

type TokenStore struct {
	byToken map[string]TokenEntry
}

func LoadTokenStore(path string) (*TokenStore, error) {
	payload, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read token file: %w", err)
	}

	var tf tokenFile
	if err := json.Unmarshal(payload, &tf); err != nil {
		return nil, fmt.Errorf("parse token file: %w", err)
	}

	store := &TokenStore{byToken: make(map[string]TokenEntry, len(tf.Tokens))}
	for _, token := range tf.Tokens {
		tokenValue := strings.TrimSpace(token.Token)
		if tokenValue == "" {
			return nil, fmt.Errorf("token file contains empty token")
		}

		normalizedSubdomain := NormalizeSubdomain(token.Subdomain)
		if normalizedSubdomain != "" && !IsValidSubdomain(normalizedSubdomain) {
			return nil, fmt.Errorf("invalid subdomain in token file for token %q", tokenValue)
		}

		if _, exists := store.byToken[tokenValue]; exists {
			return nil, fmt.Errorf("duplicate token in token file: %q", tokenValue)
		}

		store.byToken[tokenValue] = TokenEntry{
			Token:     tokenValue,
			Subdomain: normalizedSubdomain,
		}
	}

	return store, nil
}

func (s *TokenStore) Validate(token string, requestedSubdomain string) (string, bool) {
	entry, ok := s.byToken[strings.TrimSpace(token)]
	if !ok {
		return "", false
	}

	requested := NormalizeSubdomain(requestedSubdomain)
	configured := NormalizeSubdomain(entry.Subdomain)

	switch {
	case configured != "" && requested != "" && configured != requested:
		return "", false
	case configured != "":
		return configured, true
	case requested != "":
		if !IsValidSubdomain(requested) {
			return "", false
		}
		return requested, true
	default:
		derived := DeriveSubdomain(entry.Token)
		if !IsValidSubdomain(derived) {
			return "", false
		}
		return derived, true
	}
}

func IsValidSubdomain(subdomain string) bool {
	return subdomainPattern.MatchString(subdomain)
}

func NormalizeSubdomain(subdomain string) string {
	return strings.ToLower(strings.TrimSpace(subdomain))
}

func DeriveSubdomain(token string) string {
	cleaned := strings.ToLower(strings.TrimSpace(token))
	replacer := strings.NewReplacer("-", "", "_", "", ".", "", ":", "")
	cleaned = replacer.Replace(cleaned)
	if len(cleaned) >= 12 {
		cleaned = cleaned[:12]
	}
	if cleaned == "" {
		return "agent"
	}
	return cleaned
}
