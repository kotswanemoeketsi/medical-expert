package notify

import (
	"crypto/rand"
	"encoding/base64"
	"strings"
	"unicode"

	"github.com/yourorg/medical-expert-system/internal/model"
)

// The replies that are read, word for word. Anything else — "I'm not sure,
// can I take it later?", "yes", "no" — is kept for staff to read and changes
// nothing: guessing would put a confident wrong entry in a clinical record,
// and an early version of this matched the word "not", so "I'm not sure"
// was recorded as a skipped dose.
var replyPhrases = map[string]model.ReplyInterpretation{
	"taken": model.ReplyTaken, "taken it": model.ReplyTaken, "took it": model.ReplyTaken,
	"i took it": model.ReplyTaken, "i have taken it": model.ReplyTaken, "i ve taken it": model.ReplyTaken,
	"i've taken it": model.ReplyTaken, "done": model.ReplyTaken,

	"skip": model.ReplySkipped, "skipped": model.ReplySkipped, "skipping": model.ReplySkipped,
	"skip it": model.ReplySkipped, "skipped it": model.ReplySkipped, "not taken": model.ReplySkipped,
	"did not take it": model.ReplySkipped, "didn't take it": model.ReplySkipped, "didnt take it": model.ReplySkipped,

	"stop": model.ReplyStop, "stop all": model.ReplyStop, "unsubscribe": model.ReplyStop,
	"opt out": model.ReplyStop, "optout": model.ReplyStop, "stop reminders": model.ReplyStop,
	"stop messages": model.ReplyStop,
}

// canonical lowercases, keeps letters, digits and apostrophes, and collapses
// everything else to single spaces, so "TAKEN!", " taken " and "Taken ✅"
// read the same.
func canonical(s string) string {
	s = strings.NewReplacer("’", "'", "‘", "'").Replace(strings.ToLower(s))
	var b strings.Builder
	space := false
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '\'' {
			if space && b.Len() > 0 {
				b.WriteByte(' ')
			}
			space = false
			b.WriteRune(r)
			continue
		}
		space = true
	}
	return b.String()
}

// InterpretReply reads a typed reply. Only whole-message matches count.
func InterpretReply(text string) model.ReplyInterpretation {
	if kind, ok := replyPhrases[canonical(text)]; ok {
		return kind
	}
	return model.ReplyUnrecognised
}

// Button payloads are "<reply token>:<answer>".
const (
	answerTaken = "taken"
	answerSkip  = "skip"
)

// NewReplyToken returns 128 random bits, URL-safe: 22 characters.
func NewReplyToken() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// DosePayloads are the payloads for the Taken and Skip buttons.
func DosePayloads(token string) []string {
	return []string{token + ":" + answerTaken, token + ":" + answerSkip}
}

// ParsePayload reads a button payload.
func ParsePayload(p string) (token string, answer model.ReplyInterpretation, ok bool) {
	token, a, found := strings.Cut(strings.TrimSpace(p), ":")
	if !found || len(token) != 22 {
		return "", "", false
	}
	switch a {
	case answerTaken:
		return token, model.ReplyTaken, true
	case answerSkip:
		return token, model.ReplySkipped, true
	}
	return "", "", false
}
