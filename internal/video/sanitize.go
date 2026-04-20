package video

import (
	"fmt"
	"strings"
	"unicode"
)

func sanitizeVideoID(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", fmt.Errorf("videoId cannot be empty")
	}

	var builder strings.Builder
	for _, r := range raw {
		switch {
		case unicode.IsLetter(r), unicode.IsDigit(r):
			builder.WriteRune(unicode.ToLower(r))
		case r == '-', r == '_':
			builder.WriteRune(r)
		default:
			return "", fmt.Errorf("videoId contains unsupported character %q", r)
		}
	}

	return builder.String(), nil
}
