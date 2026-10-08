package model

import (
	"strings"
	"unicode"
)

const MaxTitleRunes = 80

func DeriveTitle(explicit, objective, featureKey string) string {
	if title := strings.TrimSpace(explicit); title != "" {
		return title
	}
	if title := normalizeTitleText(objective); title != "" {
		return boundTitle(title)
	}
	return boundTitle(humanizeFeatureKey(featureKey))
}

func normalizeTitleText(value string) string {
	return strings.Join(strings.Fields(value), " ")
}

func boundTitle(value string) string {
	runes := []rune(value)
	if len(runes) <= MaxTitleRunes {
		return value
	}
	return string(runes[:MaxTitleRunes-1]) + "…"
}

func humanizeFeatureKey(value string) string {
	value = strings.Map(func(char rune) rune {
		if char == '-' || char == '_' || char == '.' || char == '/' {
			return ' '
		}
		return char
	}, value)
	value = normalizeTitleText(value)
	runes := []rune(value)
	if len(runes) == 0 {
		return ""
	}
	runes[0] = unicode.ToUpper(runes[0])
	return string(runes)
}
