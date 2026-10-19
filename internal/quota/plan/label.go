package plan

import "strings"

// PlanLabel turns a provider's plan id into the label a person should see:
// "self_serve_business_prolite" becomes "Business Prolite". It is the one
// formatter every consumer uses, so the CLI and the GUI name a plan the same
// way.
//
// Only ASCII words are title-cased, so a label such as "体验版" is returned as
// is. The id "credits" is empty because a credit balance says nothing about the
// subscription tier.
func PlanLabel(raw string) string {
	p := strings.TrimPrefix(strings.ToLower(strings.TrimSpace(raw)), "self_serve_")
	if p == "credits" {
		return ""
	}
	words := strings.FieldsFunc(p, func(r rune) bool { return r == '_' || r == '-' || r == ' ' })
	for i, w := range words {
		if len(w) <= 2 && strings.ContainsAny(w, "0123456789") {
			continue // "5x", "20x"
		}
		// Title-case Latin words only; slicing the first byte of a non-ASCII
		// word would cut a character in half.
		if c := w[0]; c >= 'a' && c <= 'z' {
			words[i] = string(c-'a'+'A') + w[1:]
		}
	}
	return strings.Join(words, " ")
}
